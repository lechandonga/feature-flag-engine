package engine

import (
	"sort"
	"testing"
)

const evalConfigV2 = `{
  "version": 2,
  "flags": {
    "exp": {
      "default_variation": "off",
      "variations": ["off", "a", "b"],
      "rules": [
        {"id": "vip", "priority": 1, "match": [{"attribute": "tier", "operator": "eq", "values": ["vip"]}], "variation": "b"},
        {"id": "rollout", "priority": 10,
          "bucket": {"variations": ["off", "a"], "weights": [5000, 5000]}}
      ]
    },
    "gated": {
      "default_variation": "off",
      "variations": ["off", "on"],
      "prerequisites": [{"flag": "exp", "variation": "a"}],
      "rules": [
        {"id": "on", "variation": "on"}
      ]
    }
  }
}`

func evalEngine(t *testing.T) *Engine {
	t.Helper()
	e := New()
	if err := e.LoadConfig(mustParse(t, evalConfigV2)); err != nil {
		t.Fatal(err)
	}
	return e
}

// evalAll 对一批用户求值，返回 userID -> variation。
func evalAll(t *testing.T, cfg *Config, flag string, users []string) map[string]string {
	t.Helper()
	ev := newEvaluator(cfg)
	out := make(map[string]string, len(users))
	for _, id := range users {
		r := ev.evaluate(flag, User{ID: id}, nil)
		if r.Status != StatusOK {
			t.Fatalf("unexpected status %s for %s", r.Status, id)
		}
		out[id] = r.Variation
	}
	return out
}

func sampleUsers(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = "user-" + itoa(i)
	}
	return out
}

func TestStableAcrossEvaluationOrder(t *testing.T) {
	cfg := mustParse(t, evalConfigV2)
	users := sampleUsers(500)

	forward := evalAll(t, cfg, "exp", users)
	// 打乱顺序
	reversed := append([]string(nil), users...)
	sort.Strings(reversed)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	backward := evalAll(t, cfg, "exp", reversed)

	for id, v := range forward {
		if backward[id] != v {
			t.Fatalf("user %s variation %s vs %s under different order", id, v, backward[id])
		}
	}
}

func TestStableAcrossRuleReorder(t *testing.T) {
	cfg1 := mustParse(t, evalConfigV2)
	users := sampleUsers(500)
	before := evalAll(t, cfg1, "exp", users)

	// 重新解析同一份配置，手动打乱规则书写顺序后再次归一化（直接调换内部规则顺序，
	// 再走一次 ValidateConfig 模拟“配置重排后下发”）。
	cfg2 := mustParse(t, evalConfigV2)
	f := cfg2.Flags["exp"]
	f.Rules[0], f.Rules[1] = f.Rules[1], f.Rules[0]
	if err := ValidateConfig(cfg2); err != nil {
		t.Fatal(err)
	}
	after := evalAll(t, cfg2, "exp", users)

	for id, v := range before {
		if after[id] != v {
			t.Fatalf("user %s changed after rule reorder: %s -> %s", id, v, after[id])
		}
	}
}

// TestStableAcrossRestart 模拟进程重启：重新解析配置、重建引擎，结果必须一致。
func TestStableAcrossRestart(t *testing.T) {
	users := sampleUsers(500)
	e1 := evalEngine(t)
	first := map[string]string{}
	for _, id := range users {
		first[id] = e1.Evaluate("exp", User{ID: id}).Variation
	}
	_ = e1.Close()

	e2 := evalEngine(t)
	for _, id := range users {
		if got := e2.Evaluate("exp", User{ID: id}).Variation; got != first[id] {
			t.Fatalf("user %s changed after restart: %s -> %s", id, first[id], got)
		}
	}
	_ = e2.Close()
}

func TestPriorityTargetingWinsOverRollout(t *testing.T) {
	e := evalEngine(t)
	defer e.Close()
	r := e.Evaluate("exp", User{ID: "u9", Attributes: map[string]string{"tier": "vip"}})
	if r.Variation != "b" || r.Source != SourceRule || r.HitRuleID != "vip" {
		t.Fatalf("vip targeting result = %+v", r)
	}
}

func TestBucketResultIsExplainable(t *testing.T) {
	e := evalEngine(t)
	defer e.Close()
	r := e.Evaluate("exp", User{ID: "u9"})
	if r.Source != SourceBucket || r.HitRuleID != "rollout" || r.BucketKey == "" {
		t.Fatalf("bucket result not explainable: %+v", r)
	}
	if r.ConfigVersion != 2 {
		t.Fatalf("config version = %d", r.ConfigVersion)
	}
}

func TestDefaultWhenNoRuleMatches(t *testing.T) {
	cfg := mustParse(t, `{"version":2,"flags":{"f":{"default_variation":"off","variations":["off","on"]}}}`)
	ev := newEvaluator(cfg)
	r := ev.evaluate("f", User{ID: "x"}, nil)
	if r.Source != SourceDefault || r.Variation != "off" {
		t.Fatalf("got %+v", r)
	}
}

func TestPrerequisiteMetAndNotMet(t *testing.T) {
	e := evalEngine(t)
	defer e.Close()

	// 找到一个在 exp 分桶中落入 "a" 的用户与一个落入 "off" 的用户。
	var userA, userOff string
	for i := 0; i < 10000; i++ {
		id := "probe-" + itoa(i)
		v := e.Evaluate("exp", User{ID: id}).Variation
		if v == "a" && userA == "" {
			userA = id
		}
		if v == "off" && userOff == "" {
			userOff = id
		}
		if userA != "" && userOff != "" {
			break
		}
	}
	if userA == "" || userOff == "" {
		t.Fatal("failed to find probing users for both buckets")
	}

	ok := e.Evaluate("gated", User{ID: userA})
	if ok.Status != StatusOK || ok.Variation != "on" {
		t.Fatalf("prereq met result = %+v", ok)
	}
	blocked := e.Evaluate("gated", User{ID: userOff})
	if blocked.Status != StatusPrerequisiteNotMet || blocked.Variation != "off" ||
		blocked.Source != SourcePrerequisiteDefault {
		t.Fatalf("prereq not-met result = %+v", blocked)
	}
}

func TestFlagNotFoundAndUnavailable(t *testing.T) {
	e := evalEngine(t)
	defer e.Close()
	r := e.Evaluate("missing", User{ID: "u"})
	if r.Status != StatusFlagNotFound {
		t.Fatalf("got %+v", r)
	}

	empty := New()
	defer empty.Close()
	r2 := empty.Evaluate("exp", User{ID: "u"})
	if r2.Status != StatusUnavailable || r2.Reason != ReasonConfigUnavailable {
		t.Fatalf("got %+v", r2)
	}
}
