package flag

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func goodPayload(version Version) []byte {
	// version field is informational here; envelope version stays v2 but we
	// distinguish snapshots by a unique flag key per revision.
	return []byte(`{"version":"v2","flags":[
	  {"key":"f","on":true,"rules":[
	    {"id":"r","priority":1,"percentage":100,
	     "variants":[{"key":"v-` + string(version) + `","weightBPS":10000}]}]}]}`)
}

func TestRefresh_FailureKeepsLastGoodConfig(t *testing.T) {
	var calls atomic.Int32
	payloads := [][]byte{
		goodPayload("rev1"),
		[]byte(`{broken`),                     // parse failure
		nil,                                   // load failure
		[]byte(`{"version":"v2","flags":[]}`), // empty -> rejected
		goodPayload("rev2"),
	}
	loader := LoaderFunc(func(ctx context.Context) ([]byte, error) {
		i := int(calls.Add(1)) - 1
		p := payloads[i%len(payloads)]
		if p == nil {
			return nil, errors.New("network down")
		}
		return p, nil
	})
	e := NewEngine()
	r := NewRefresher(e, loader)
	ctx := context.Background()

	if err := r.RefreshOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := e.Evaluate(ctx, User{ID: "u1"}, "f")
	if got.Variant != "v-rev1" {
		t.Fatalf("initial: %+v", got)
	}

	// Two failed refreshes: evaluation must stay on rev1.
	if err := r.RefreshOnce(ctx); err == nil {
		t.Fatal("expected parse error")
	}
	if err := r.RefreshOnce(ctx); err == nil {
		t.Fatal("expected load error")
	}
	if err := r.RefreshOnce(ctx); err == nil {
		t.Fatal("expected empty-config error")
	}
	got = e.Evaluate(ctx, User{ID: "u1"}, "f")
	if got.Variant != "v-rev1" {
		t.Fatalf("last good config not retained: %+v", got)
	}
	st := r.Stats()
	if st.LastOK || st.Failures != 3 || st.LastVersion != Version2 {
		t.Fatalf("stats wrong: %+v", st)
	}

	// Recovery.
	if err := r.RefreshOnce(ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	got = e.Evaluate(ctx, User{ID: "u1"}, "f")
	if got.Variant != "v-rev2" {
		t.Fatalf("recovery: %+v", got)
	}
}

func TestRefresh_UnchangedPayloadSkipped(t *testing.T) {
	var loads atomic.Int32
	loader := LoaderFunc(func(ctx context.Context) ([]byte, error) {
		loads.Add(1)
		return goodPayload("rev1"), nil
	})
	e := NewEngine()
	r := NewRefresher(e, loader)
	for i := 0; i < 3; i++ {
		if err := r.RefreshOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	st := r.Stats()
	if loads.Load() != 3 || st.Successes != 1 {
		t.Fatalf("expected 3 loads but 1 install, got loads=%d successes=%d", loads.Load(), st.Successes)
	}
}

func TestRefresh_BackgroundLoopAndFailureDegradation(t *testing.T) {
	var good atomic.Bool
	good.Store(true)
	loader := LoaderFunc(func(ctx context.Context) ([]byte, error) {
		if !good.Load() {
			return nil, errors.New("500")
		}
		return goodPayload("rev1"), nil
	})
	e := NewEngine()
	r := NewRefresher(e, loader)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx, 10*time.Millisecond)
	defer r.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := e.Evaluate(ctx, User{ID: "u1"}, "f"); got.On && got.Variant == "v-rev1" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := e.Evaluate(ctx, User{ID: "u1"}, "f")
	if !got.On {
		t.Fatalf("initial config never loaded: %+v", got)
	}

	// Loader starts failing; evaluations must keep succeeding on last good.
	good.Store(false)
	time.Sleep(80 * time.Millisecond)
	for i := 0; i < 50; i++ {
		got := e.Evaluate(ctx, User{ID: "u1"}, "f")
		if !got.On || got.Variant != "v-rev1" {
			t.Fatalf("jitter during failing refresh: %+v", got)
		}
	}
}

func TestConcurrentEvaluation_NoJitterAcrossRefresh(t *testing.T) {
	revision := atomic.Int32{}
	revision.Store(1)
	// Payload changes byte-for-byte each refresh (description embeds the
	// revision) but is semantically identical: the same rule, percentage and
	// weights. Bucketing inputs never include revision, so results must not move.
	loader := LoaderFunc(func(ctx context.Context) ([]byte, error) {
		v := revision.Load()
		return []byte(`{"version":"v2","flags":[{"key":"f","on":true,"description":"rev-` +
			itoaTest(int(v)) + `","rules":[
		  {"id":"r","priority":1,"percentage":100,
		   "variants":[{"key":"v","weightBPS":10000}]}]}]}`), nil
	})
	e := NewEngine()
	r := NewRefresher(e, loader)
	if err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	const users = 200
	users_ := make([]User, users)
	want := make([]Result, users)
	for i := range users_ {
		users_[i] = User{ID: "user-" + itoaTest(i)}
		want[i] = e.Evaluate(context.Background(), users_[i], "f")
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				revision.Add(1)
				_ = r.RefreshOnce(context.Background())
			}
		}
	}()
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for k := 0; k < 200; k++ {
				idx := (seed + k) % users
				got := e.Evaluate(context.Background(), users_[idx], "f")
				if got.Variant != want[idx].Variant || got.On != want[idx].On ||
					got.Status != StatusOn || got.MatchedRule != "r" {
					t.Errorf("jitter for %s: %+v", users_[idx].ID, got)
					return
				}
			}
		}(g)
	}
	// Let the churn run briefly, then stop the refresher and wait for workers.
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestEvaluate_BeforeFirstConfig_IsUnavailable(t *testing.T) {
	e := NewEngine()
	got := e.Evaluate(context.Background(), User{ID: "u"}, "f")
	if got.Status != StatusUnavailable || got.Source != SourceConfigUnavailable {
		t.Fatalf("want unavailable, got %+v", got)
	}
}
