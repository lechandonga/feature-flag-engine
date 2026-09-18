package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// controllableSource 是测试用数据源，可随时切换返回内容或注入失败。
type controllableSource struct {
	mu     sync.Mutex
	data   []byte
	source string
	fail   bool
	calls  int64
}

func (s *controllableSource) Load(_ context.Context) ([]byte, string, error) {
	atomic.AddInt64(&s.calls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, "", errors.New("simulated source outage")
	}
	return append([]byte(nil), s.data...), s.source, nil
}

func (s *controllableSource) set(data string) {
	s.mu.Lock()
	s.data = []byte(data)
	s.fail = false
	s.mu.Unlock()
}

func (s *controllableSource) setFail() {
	s.mu.Lock()
	s.fail = true
	s.mu.Unlock()
}

// goodCfgV1Off / goodCfgV2On 同时覆盖 v1 -> v2 的在线升级场景。
const goodCfgV1Off = `{"version":1,"flags":{"f":{"on":false,"default":"off","allowed_values":["off","on"]}}}`
const goodCfgV2On = `{"version":2,"flags":{
  "f":{"default_variation":"off","variations":["off","on"],
       "rules":[{"id":"all-on","variation":"on"}]}
}}`

func TestRefreshSuccessAtomicallySwaps(t *testing.T) {
	src := &controllableSource{data: []byte(goodCfgV1Off), source: "test"}
	e := New()
	defer e.Close()

	if err := e.Load(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if r := e.Evaluate("f", User{ID: "u1"}); r.Variation != "off" {
		t.Fatalf("before refresh = %+v", r)
	}

	src.set(goodCfgV2On)
	if err := e.refreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.IsStale() {
		t.Fatal("should not be stale after successful refresh")
	}
	if r := e.Evaluate("f", User{ID: "u1"}); r.Variation != "on" {
		t.Fatalf("after refresh = %+v", r)
	}
	if e.LastRefreshError() != nil {
		t.Fatalf("unexpected last error: %v", e.LastRefreshError())
	}
}

func TestRefreshFailureKeepsLastGoodConfig(t *testing.T) {
	src := &controllableSource{data: []byte(goodCfgV1Off), source: "test"}
	e := New()
	defer e.Close()
	if err := e.Load(context.Background(), src); err != nil {
		t.Fatal(err)
	}

	// 1) 下发一份坏配置（缺依赖）：必须被拒绝，旧行为不变。
	src.set(`{"version":2,"flags":{"f":{"default_variation":"off","variations":["off"],
	  "prerequisites":[{"flag":"ghost","variation":"on"}]}}}`)
	err := e.refreshOnce(context.Background())
	if err == nil {
		t.Fatal("expected rejection")
	}
	if ce := e.LastRefreshError(); ce == nil || ce.Reason != ReasonMissingDependency {
		t.Fatalf("last error reason = %v", ce)
	}
	if !e.IsStale() {
		t.Fatal("snapshot should be marked stale after failed refresh")
	}
	if r := e.Evaluate("f", User{ID: "u1"}); r.Status != StatusOK || !r.Stale || r.Variation != "off" {
		t.Fatalf("evaluation after bad config = %+v", r)
	}

	// 2) 数据源直接故障：仍沿用旧配置。
	src.setFail()
	if err := e.refreshOnce(context.Background()); err == nil {
		t.Fatal("expected source failure error")
	}
	if r := e.Evaluate("f", User{ID: "u1"}); r.Variation != "off" || !r.Stale {
		t.Fatalf("evaluation during outage = %+v", r)
	}

	// 3) 恢复并下发好配置：stale 清除，行为更新。
	src.set(goodCfgV2On)
	if err := e.refreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.IsStale() {
		t.Fatal("stale flag should clear after recovery")
	}
	if r := e.Evaluate("f", User{ID: "u1"}); r.Variation != "on" {
		t.Fatalf("evaluation after recovery = %+v", r)
	}
}

func TestInitialLoadFailureMeansUnavailable(t *testing.T) {
	src := &controllableSource{fail: true}
	e := New()
	defer e.Close()
	err := e.Load(context.Background(), src)
	if err == nil {
		t.Fatal("expected load error")
	}
	r := e.Evaluate("f", User{ID: "u"})
	if r.Status != StatusUnavailable {
		t.Fatalf("got %+v", r)
	}
}

// TestConcurrentEvaluationDuringRefresh 并发评估 + 高频刷新交替成功/失败，
// 任何一次评估都必须基于完整快照，不允许出现空结果或瞬时失效。
func TestConcurrentEvaluationDuringRefresh(t *testing.T) {
	src := &controllableSource{data: []byte(goodCfgV1Off), source: "test"}
	e := New()
	if err := e.Load(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 刷新协程：好配置 / 坏配置 / 故障交替。
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		n := 0
		for {
			select {
			case <-tick.C:
				switch n % 3 {
				case 0:
					src.set(goodCfgV2On)
				case 1:
					src.set(`{"version":2,"flags":{"f":{}}}`) // invalid: missing default
				case 2:
					src.setFail()
				}
				_ = e.refreshOnce(context.Background())
				n++
			case <-stop:
				return
			}
		}
	}()

	// 评估协程们：结果只可能是 on / off + 有效 status，绝不能为空。
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				r := e.Evaluate("f", User{ID: fmt.Sprintf("g%d-u%d", g, i%50)})
				if r.Status != StatusOK {
					t.Errorf("unexpected status %s during refresh", r.Status)
					return
				}
				if r.Variation != "on" && r.Variation != "off" {
					t.Errorf("invalid variation %q during refresh", r.Variation)
					return
				}
			}
		}(g)
	}

	time.Sleep(120 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestStableUnderConcurrentRefresh 同一用户在刷新风暴中结果只能是两种合法快照结果
// 之一（取决于读到哪个版本），且同快照内多次结果完全一致。
func TestStableUnderConcurrentRefresh(t *testing.T) {
	src := &controllableSource{data: []byte(goodCfgV1Off), source: "test"}
	e := New()
	if err := e.Load(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	go func() {
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				src.set(goodCfgV1Off)
			} else {
				src.set(goodCfgV2On)
			}
			_ = e.refreshOnce(context.Background())
		}
	}()

	byVersion := map[int]string{}
	for i := 0; i < 5000; i++ {
		r := e.Evaluate("f", User{ID: "fixed-user"})
		want := byVersion[r.ConfigVersion]
		if want == "" {
			byVersion[r.ConfigVersion] = r.Variation
			continue
		}
		if r.Variation != want {
			t.Fatalf("same version %d yielded %s and %s", r.ConfigVersion, want, r.Variation)
		}
	}
}

func TestBackgroundRefresherLifecycle(t *testing.T) {
	src := &controllableSource{data: []byte(goodCfgV1Off), source: "test"}
	e := New()
	if err := e.StartRefresher(context.Background(), src, 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	src.set(goodCfgV2On)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.Evaluate("f", User{ID: "u1"}).Variation == "on" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := e.Evaluate("f", User{ID: "u1"}).Variation; got != "on" {
		t.Fatalf("background refresh did not take effect: %s", got)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}
