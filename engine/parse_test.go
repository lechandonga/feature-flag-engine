package engine

import (
	"testing"
)

func mustParse(t *testing.T, data string) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(data), "test")
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	return cfg
}

func assertReason(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with reason %s, got nil", want)
	}
	ce, ok := err.(*ConfigError)
	if !ok {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}
	if ce.Reason != want {
		t.Fatalf("reason = %s, want %s (path=%s msg=%s)", ce.Reason, want, ce.Path, ce.Msg)
	}
}

func TestParseV1Defaults(t *testing.T) {
	// version 缺省 => v1；无任何字段 => off。
	cfg := mustParse(t, `{"flags":{"f1":{}}}`)
	if cfg.Version != 1 {
		t.Fatalf("version = %d, want 1", cfg.Version)
	}
	f := cfg.Flags["f1"]
	if f.DefaultVariation != "off" {
		t.Fatalf("default = %q, want off", f.DefaultVariation)
	}
	if len(f.Rules) != 0 {
		t.Fatalf("expected no rules, got %d", len(f.Rules))
	}
}

func TestParseV1OnAndWhitelist(t *testing.T) {
	data := `{
	  "flags": {
	    "f1": {
	      "on": true,
	      "variation": "on",
	      "default": "off",
	      "whitelist": {"vip-1": "canary"},
	      "allowed_values": ["off", "on", "canary"]
	    }
	  }
	}`
	cfg := mustParse(t, data)
	f := cfg.Flags["f1"]
	if len(f.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(f.Rules))
	}
	// whitelist 规则优先级更高（数字更小）。
	if f.Rules[0].ID != "v1-whitelist-0" || f.Rules[0].Priority != 10 {
		t.Fatalf("first rule = %+v", f.Rules[0])
	}
	if f.Rules[1].ID != "v1-on" || f.Rules[1].Priority != 100 {
		t.Fatalf("second rule = %+v", f.Rules[1])
	}
}

func TestParseV2Basic(t *testing.T) {
	data := `{
	  "version": 2,
	  "flags": {
	    "checkout-new": {
	      "default_variation": "off",
	      "variations": ["off", "on"],
	      "rules": [
	        {"id": "rollout", "bucket": {"variations": ["off","on"], "weights": [9000,1000]}}
	      ]
	    }
	  }
	}`
	cfg := mustParse(t, data)
	if cfg.Version != 2 {
		t.Fatalf("version = %d", cfg.Version)
	}
	f := cfg.Flags["checkout-new"]
	r := f.Rules[0]
	if r.Priority != 100 {
		t.Fatalf("default priority = %d, want 100", r.Priority)
	}
	if r.Bucket == nil || r.Bucket.Weights[1] != 1000 {
		t.Fatalf("bucket not normalized: %+v", r.Bucket)
	}
}

func TestV1WhitelistRuleIDsStableAcrossParses(t *testing.T) {
	data := `{"version":1,"flags":{"f":{"on":true,"variation":"on",
	  "whitelist":{"zeta":"on","alpha":"on","mid":"on"},"allowed_values":["off","on"]}}}`
	var firstIDs []string
	for i := 0; i < 5; i++ {
		cfg := mustParse(t, data)
		var ids []string
		for _, r := range cfg.Flags["f"].Rules {
			if len(r.Match) == 0 {
				ids = append(ids, r.ID+"=<all>")
				continue
			}
			ids = append(ids, r.ID+"="+r.Match[0].Values[0])
		}
		if firstIDs == nil {
			firstIDs = ids
			continue
		}
		if len(ids) != len(firstIDs) {
			t.Fatal("rule count differs")
		}
		for j := range ids {
			if ids[j] != firstIDs[j] {
				t.Fatalf("v1 rule ids not stable: %v vs %v", ids, firstIDs)
			}
		}
	}
}

func TestParseUnknownVersionRejected(t *testing.T) {
	_, err := ParseConfig([]byte(`{"version":99,"flags":{}}`), "t")
	assertReason(t, err, ReasonUnsupportedVersion)
}

func TestParseUnknownFieldsIgnored(t *testing.T) {
	// 未知顶层/字段应被忽略而不是拒绝（前向兼容）。
	cfg := mustParse(t, `{"version":2,"future":"x","flags":{"f":{"default_variation":"off","variations":["off"],"unknown":123}}}`)
	if cfg.Version != 2 {
		t.Fatal("version mismatch")
	}
}

func TestParseInvalidValuesRejected(t *testing.T) {
	// 权重和不为 10000
	_, err := ParseConfig([]byte(`{"version":2,"flags":{"f":{
		"default_variation":"off","variations":["off","on"],
		"rules":[{"id":"r","bucket":{"variations":["off","on"],"weights":[50,50]}}]
	}}}`), "t")
	assertReason(t, err, ReasonInvalidField)

	// 规则既给 variation 又给 bucket => contradiction
	_, err = ParseConfig([]byte(`{"version":2,"flags":{"f":{
		"default_variation":"off","variations":["off"],
		"rules":[{"id":"r","variation":"off","bucket":{"variations":["off"],"weights":[10000]}}]
	}}}`), "t")
	assertReason(t, err, ReasonContradiction)

	// 未知操作符
	_, err = ParseConfig([]byte(`{"version":2,"flags":{"f":{
		"default_variation":"off","variations":["off"],
		"rules":[{"id":"r","variation":"off","match":[{"attribute":"uid","operator":"regex","values":["a"]}]}]
	}}}`), "t")
	assertReason(t, err, ReasonInvalidField)

	// 变体未在 variations 中声明
	_, err = ParseConfig([]byte(`{"version":2,"flags":{"f":{
		"default_variation":"off","variations":["off"],
		"rules":[{"id":"r","variation":"on"}]
	}}}`), "t")
	assertReason(t, err, ReasonInvalidField)

	// 重复规则 id
	_, err = ParseConfig([]byte(`{"version":2,"flags":{"f":{
		"default_variation":"off","variations":["off"],
		"rules":[{"id":"r","variation":"off"},{"id":"r","variation":"off"}]
	}}}`), "t")
	assertReason(t, err, ReasonDuplicateDefinition)
}

func TestMissingDependencyRejected(t *testing.T) {
	_, err := ParseConfig([]byte(`{"version":2,"flags":{"f":{
		"default_variation":"off","variations":["off"],
		"prerequisites":[{"flag":"ghost","variation":"on"}]
	}}}`), "t")
	assertReason(t, err, ReasonMissingDependency)
}

func TestSelfDependencyRejected(t *testing.T) {
	_, err := ParseConfig([]byte(`{"version":2,"flags":{"f":{
		"default_variation":"off","variations":["off","on"],
		"prerequisites":[{"flag":"f","variation":"on"}]
	}}}`), "t")
	assertReason(t, err, ReasonCycleDependency)
}

func TestCycleDependencyRejected(t *testing.T) {
	// a -> b -> c -> a
	data := `{"version":2,"flags":{
	  "a":{"default_variation":"off","variations":["off","on"],"prerequisites":[{"flag":"b","variation":"on"}]},
	  "b":{"default_variation":"off","variations":["off","on"],"prerequisites":[{"flag":"c","variation":"on"}]},
	  "c":{"default_variation":"off","variations":["off","on"],"prerequisites":[{"flag":"a","variation":"on"}]}
	}}`
	_, err := ParseConfig([]byte(data), "t")
	assertReason(t, err, ReasonCycleDependency)
}

func TestContradictionPrerequisiteImpossible(t *testing.T) {
	// b 永远不会产生 "on"（没有规则，默认 off），a 却要求 b=on
	_, err := ParseConfig([]byte(`{"version":2,"flags":{
	  "a":{"default_variation":"off","variations":["off","on"],"prerequisites":[{"flag":"b","variation":"on"}]},
	  "b":{"default_variation":"off","variations":["off"]}
	}}`), "t")
	assertReason(t, err, ReasonContradiction)
}

func TestRejectedConfigDoesNotMutateOldOne(t *testing.T) {
	e := New()
	good := `{"version":2,"flags":{"f":{"default_variation":"off","variations":["off","on"]}}}`
	if err := e.LoadConfig(mustParse(t, good)); err != nil {
		t.Fatal(err)
	}
	bad := `{"version":2,"flags":{"f":{"default_variation":"off","variations":["off"],"prerequisites":[{"flag":"x","variation":"on"}]}}}`
	_, err := ParseConfig([]byte(bad), "t")
	if err == nil {
		t.Fatal("expected bad config rejected")
	}
	// 引擎版本与行为保持原样。
	if e.CurrentVersion() != 2 {
		t.Fatalf("version changed: %d", e.CurrentVersion())
	}
	r := e.Evaluate("f", User{ID: "u1"})
	if r.Status != StatusOK || r.Variation != "off" {
		t.Fatalf("evaluation after rejection changed: %+v", r)
	}
}
