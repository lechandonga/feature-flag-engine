package flag

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestV1_LegacyOrderAndWeight_Migrated(t *testing.T) {
	// v1 payload: "order" instead of priority, "weight" in percentage units,
	// flag-level percent rollout.
	payload := []byte(`{
	  "version": "v1",
	  "flags": [
	    {"key": "old-exp", "on": true, "percent": 100,
	     "rules": []},
	    {"key": "old-rules", "on": true, "rules": [
	      {"id": "r1", "order": 2,
	       "weight": [{"variant": "a", "percent": 70}, {"variant": "b", "percent": 30}]},
	      {"id": "r2", "order": 1}
	    ]}
	  ]
	}`)
	cfg, err := ParseConfig(payload)
	if err != nil {
		t.Fatalf("v1 parse: %v", err)
	}
	if cfg.Version != Version2 {
		t.Fatalf("migrated version = %q", cfg.Version)
	}
	r1 := cfg.Flags["old-rules"].Rules[0] // rules stay in listed order; priority used at match time
	if r1.Priority != 2 {
		t.Fatalf("order not migrated: %+v", r1)
	}
	// find r1 regardless of slice position
	var byID *Rule
	for _, r := range cfg.Flags["old-rules"].Rules {
		if r.ID == "r1" {
			byID = r
		}
	}
	if byID == nil || len(byID.Variants) != 2 || byID.Variants[0].WeightBPS != 7000 || byID.Variants[1].WeightBPS != 3000 {
		t.Fatalf("weight migration failed: %+v", byID)
	}
	// r2 had no variants -> implicit single "on" variant at full weight
	var r2 *Rule
	for _, r := range cfg.Flags["old-rules"].Rules {
		if r.ID == "r2" {
			r2 = r
		}
	}
	if r2.Percentage != 100 || len(r2.Variants) != 1 || r2.Variants[0].Key != "on" {
		t.Fatalf("default rule not applied: %+v", r2)
	}

	// Legacy config must still evaluate identically after migration.
	e := NewEngine()
	e.Snapshot(cfg)
	got := e.Evaluate(context.Background(), User{ID: "u1"}, "old-exp")
	if !got.On {
		t.Fatalf("legacy percent flag: %+v", got)
	}
	got2 := e.Evaluate(context.Background(), User{ID: "u1"}, "old-rules")
	if !got2.On || got2.MatchedRule != "r2" { // priority 1 wins
		t.Fatalf("legacy order priority: %+v", got2)
	}
}

func TestUnknownFields_Ignored(t *testing.T) {
	payload := []byte(`{
	  "version": "v2",
	  "futureEnvelope": 42,
	  "flags": [
	    {"key": "f", "on": true, "hints": {"x": true}, "owner": "team-x",
	     "rules": [{"id": "r", "priority": 1, "percentage": 100,
	                "experimentTag": "abc", "futureRollout": {"mode": "shadow"},
	                "variants": [{"key": "on", "weightBPS": 10000, "newField": 9}]}]}
	  ]
	}`)
	cfg, err := ParseConfig(payload)
	if err != nil {
		t.Fatalf("unknown fields must be ignored: %v", err)
	}
	if cfg.Version != Version2 {
		t.Fatal("version lost")
	}
}

func TestUnknownVersion_Rejected(t *testing.T) {
	for _, v := range []string{`{"version":"v9","flags":[]}`, `{"flags":[]}`} {
		if _, err := ParseConfig([]byte(v)); !errors.Is(err, ErrUnknownVersion) {
			t.Fatalf("payload %s: want unknown version, got %v", v, err)
		}
	}
}

func TestIllegalValues_RejectedWithTypedErrors(t *testing.T) {
	cases := map[error]string{
		ErrMalformedJSON: `{not json`,
		ErrEmptyConfig:   ``,
		ErrBadPercentage: `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,"percentage":150,"variants":[{"key":"a","weightBPS":10000}]}]}]}`,
		ErrWeightSum: `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,"variants":[{"key":"a","weightBPS":5000}]}]}]}`,
		ErrBadPriority: `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":0,"variants":[{"key":"a","weightBPS":10000}]}]}]}`,
		ErrUnknownOperator: `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,"conditions":[{"attribute":"a","operator":"~regex","values":["x"]}],
		   "variants":[{"key":"a","weightBPS":10000}]}]}]}`,
		ErrDuplicateFlagKey: `{"version":"v2","flags":[{"key":"f","on":true},{"key":"f","on":true}]}`,
		ErrDuplicateRuleID: `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,"variants":[{"key":"a","weightBPS":10000}]},
		  {"id":"r","priority":2,"variants":[{"key":"a","weightBPS":10000}]}]}]}`,
		ErrBadCondition: `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,"conditions":[{"attribute":"a","operator":"in","values":[]}],
		   "variants":[{"key":"a","weightBPS":10000}]}]}]}`,
		ErrIllegalValue: `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,"variants":[{"key":"a","weightBPS":-1}]}]}]}`,
	}
	for want, body := range cases {
		_, err := ParseConfig([]byte(body))
		if !errors.Is(err, want) {
			t.Fatalf("body %q: want %v, got %v", body, want, err)
		}
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("all rejections must wrap ErrInvalidConfig, got %v", err)
		}
	}
}

func TestDefaults_AppliedConsistently(t *testing.T) {
	// flag on, no rules -> implicit full on; percentage omitted -> 100.
	payload := []byte(`{"version":"v2","flags":[{"key":"f","on":true,"rules":[
	  {"id":"r","priority":1}]}]}`)
	cfg, err := ParseConfig(payload)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Flags["f"].Rules[0]
	if r.Percentage != 100 || len(r.Variants) != 1 || r.Variants[0].Key != "on" {
		t.Fatalf("defaults: %+v", r)
	}

	// off flag with no rules stays off, no implicit rule injected.
	payload2 := []byte(`{"version":"v2","flags":[{"key":"g","on":false}]}`)
	cfg2, err := ParseConfig(payload2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg2.Flags["g"].Rules) != 0 {
		t.Fatal("off flag must not gain implicit rules")
	}
}

func TestEmptyConfig_Rejected(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"version":"v2","flags":[]}`)); !errors.Is(err, ErrEmptyConfig) {
		t.Fatalf("want empty config error, got %v", err)
	}
}

func TestErrorMessages_ExplainContext(t *testing.T) {
	_, err := ParseConfig([]byte(`{"version":"v2","flags":[{"key":"f","on":true,"rules":[
	  {"id":"r","priority":1,"percentage":200,"variants":[{"key":"a","weightBPS":10000}]}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "r") {
		t.Fatalf("error should reference rule id: %v", err)
	}
}
