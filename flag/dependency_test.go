package flag

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func onFlag(key string, prereqs ...string) map[string]interface{} {
	return map[string]interface{}{
		"key": key, "on": true,
		"prerequisites": prereqs,
		"rules": []map[string]interface{}{{
			"id": key + "-r", "priority": 1, "percentage": 100,
			"variants": []map[string]interface{}{{"key": "on", "weightBPS": 10000}},
		}},
	}
}

func offFlag(key string, prereqs ...string) map[string]interface{} {
	return map[string]interface{}{"key": key, "on": false, "prerequisites": prereqs}
}

func TestMissingDependency_Rejected(t *testing.T) {
	payload := buildConfig(t, onFlag("child", "ghost"))
	_, err := ParseConfig(payload)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !errors.Is(err, ErrInvalidConfig) || !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("want ErrMissingDependency wrapped, got %v", err)
	}
}

func TestSelfDependency_RejectedAsCycle(t *testing.T) {
	payload := buildConfig(t, onFlag("self", "self"))
	_, err := ParseConfig(payload)
	if !errors.Is(err, ErrCircularDependency) {
		t.Fatalf("want ErrCircularDependency, got %v", err)
	}
}

func TestCircularDependency_TwoAndThreeNodes(t *testing.T) {
	cases := [][]map[string]interface{}{
		{onFlag("a", "b"), onFlag("b", "a")},
		{onFlag("a", "b"), onFlag("b", "c"), onFlag("c", "a")},
		{onFlag("a", "b"), onFlag("b"), onFlag("c", "c")},
	}
	for i, flags := range cases {
		payload := buildConfig(t, flags...)
		_, err := ParseConfig(payload)
		if !errors.Is(err, ErrCircularDependency) {
			t.Fatalf("case %d: want cycle, got %v", i, err)
		}
	}
}

func TestContradiction_PermanentlyOffPrereq(t *testing.T) {
	payload := buildConfig(t, onFlag("child", "base"), offFlag("base"))
	_, err := ParseConfig(payload)
	if !errors.Is(err, ErrDependencyContradiction) {
		t.Fatalf("want contradiction, got %v", err)
	}
}

func TestContradiction_DuplicatePrereq(t *testing.T) {
	payload := buildConfig(t, onFlag("child", "base", "base"), onFlag("base"))
	_, err := ParseConfig(payload)
	if !errors.Is(err, ErrDependencyContradiction) {
		t.Fatalf("want contradiction, got %v", err)
	}
}

func TestRejectionReasons_AreDistinguishable(t *testing.T) {
	cases := map[error][]map[string]interface{}{
		ErrMissingDependency:       {onFlag("c", "x")},
		ErrCircularDependency:      {onFlag("c", "c")},
		ErrDependencyContradiction: {onFlag("c", "b"), offFlag("b")},
	}
	for want, flags := range cases {
		_, err := ParseConfig(buildConfig(t, flags...))
		if !errors.Is(err, want) {
			t.Fatalf("want %v, got %v", want, err)
		}
	}
}

func TestRejectedConfig_DoesNotAffectExistingEvaluation(t *testing.T) {
	good := buildConfig(t, onFlag("base"))
	cfg, err := ParseConfig(good)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine()
	e.Snapshot(cfg)

	bad := buildConfig(t, onFlag("base", "missing"))
	if _, err := ParseConfig(bad); err == nil {
		t.Fatal("bad config should fail to parse")
	}
	// Simulate refresher behavior: only snapshot successfully parsed configs.
	badCfg, perr := ParseConfig(bad)
	if perr == nil {
		e.Snapshot(badCfg)
	}

	r := e.Evaluate(context.Background(), User{ID: "u1"}, "base")
	if !r.On || r.ConfigVersion != Version2 {
		t.Fatalf("existing evaluation changed after rejected config: %+v", r)
	}
	if e.Current() != Version2 {
		t.Fatalf("active version changed: %s", e.Current())
	}
}

func TestPrereqNotMet_GatesFlag(t *testing.T) {
	// base is ON only for tier=vip; child requires base. A non-vip user must
	// see child as prereq_not_met even though child's own rule would match.
	payload := `{
	  "version": "v2",
	  "flags": [
	    {"key": "base", "on": true, "rules": [
	      {"id": "vip", "priority": 1,
	       "conditions": [{"attribute": "tier", "operator": "eq", "value": "vip"}],
	       "percentage": 100, "variants": [{"key": "on", "weightBPS": 10000}]}
	    ]},
	    {"key": "child", "on": true, "prerequisites": ["base"], "rules": [
	      {"id": "all", "priority": 1, "percentage": 100,
	       "variants": [{"key": "on", "weightBPS": 10000}]}
	    ]}
	  ]
	}`
	cfg, err := ParseConfig([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine()
	e.Snapshot(cfg)

	got := e.Evaluate(context.Background(), User{ID: "u", Attributes: map[string]string{"tier": "normal"}}, "child")
	if got.Status != StatusPrereqNotMet || got.On {
		t.Fatalf("want prereq_not_met/off, got %+v", got)
	}
	if !strings.Contains(got.Reason, "base") {
		t.Fatalf("reason should name prerequisite: %s", got.Reason)
	}

	got2 := e.Evaluate(context.Background(), User{ID: "u", Attributes: map[string]string{"tier": "vip"}}, "child")
	if !got2.On {
		t.Fatalf("vip user should pass prereq: %+v", got2)
	}
}

func TestContradiction_OffPrereqEvenWithRules(t *testing.T) {
	// base is declared off even though it carries a rollout rule; the master
	// switch short-circuits rules, so base can never be ON.
	payload := buildConfig(t, onFlag("child", "base"),
		map[string]interface{}{
			"key": "base", "on": false,
			"rules": []map[string]interface{}{{
				"id": "base-r", "priority": 1, "percentage": 100,
				"variants": []map[string]interface{}{{"key": "on", "weightBPS": 10000}},
			}},
		})
	_, err := ParseConfig(payload)
	if !errors.Is(err, ErrDependencyContradiction) {
		t.Fatalf("want contradiction for off prerequisite with rules, got %v", err)
	}
}
