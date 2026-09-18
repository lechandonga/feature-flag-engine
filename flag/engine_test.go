package flag

import (
	"context"
	"testing"
)

const smokeConfig = `{
  "version": "v2",
  "flags": [
    {
      "key": "checkout-redesign",
      "on": true,
      "rules": [
        {
          "id": "beta-users",
          "priority": 10,
          "conditions": [{"attribute": "tier", "operator": "in", "values": ["beta"]}],
          "percentage": 100,
          "variants": [{"key": "redesign", "weightBPS": 10000}]
        },
        {
          "id": "half-rollout",
          "priority": 20,
          "conditions": [],
          "percentage": 50,
          "variants": [
            {"key": "old", "weightBPS": 5000},
            {"key": "new", "weightBPS": 5000}
          ]
        }
      ]
    }
  ]
}`

func TestSmoke(t *testing.T) {
	cfg, err := ParseConfig([]byte(smokeConfig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := NewEngine()
	e.Snapshot(cfg)

	r := e.Evaluate(context.Background(), User{ID: "u1", Attributes: map[string]string{"tier": "beta"}}, "checkout-redesign")
	if !r.On || r.Variant != "redesign" || r.MatchedRule != "beta-users" {
		t.Fatalf("beta user result: %+v", r)
	}
	t.Logf("beta: %s", r.Reason)

	r2 := e.Evaluate(context.Background(), User{ID: "u1", Attributes: map[string]string{}}, "checkout-redesign")
	t.Logf("anon: on=%v variant=%q rule=%s reason=%s", r2.On, r2.Variant, r2.MatchedRule, r2.Reason)

	// unknown flag
	r3 := e.Evaluate(context.Background(), User{ID: "u1"}, "nope")
	if r3.Status != StatusFlagNotFound {
		t.Fatalf("expected not found, got %+v", r3)
	}

	// no config
	e2 := NewEngine()
	r4 := e2.Evaluate(context.Background(), User{ID: "u1"}, "checkout-redesign")
	if r4.Status != StatusUnavailable {
		t.Fatalf("expected unavailable, got %+v", r4)
	}
}
