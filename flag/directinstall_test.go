package flag

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
)

// Hand-built equivalents of the JSON payloads used in the parity table.
// They deliberately bypass every default that normalize would inject, so the
// test proves Install enforces the strictness itself instead of relying on
// parsing having run first.
func handConfigForParity(kind string) *Config {
	validVariants := []Variant{{Key: "on", WeightBPS: 10000}}
	rule := func(conds []Condition) *Rule {
		return &Rule{ID: "r", Priority: 1, Percentage: 100,
			Conditions: conds, Variants: validVariants}
	}
	flags := map[string]*Flag{}
	switch kind {
	case "unknown-operator":
		flags["f"] = &Flag{Key: "f", On: true, Rules: []*Rule{
			rule([]Condition{{Attribute: "a", Operator: Operator("~regex"), Values: []string{"x"}}}),
		}}
	case "in-empty":
		flags["f"] = &Flag{Key: "f", On: true, Rules: []*Rule{
			rule([]Condition{{Attribute: "a", Operator: OpIn}}),
		}}
	case "notin-empty":
		flags["f"] = &Flag{Key: "f", On: true, Rules: []*Rule{
			rule([]Condition{{Attribute: "a", Operator: OpNotIn}}),
		}}
	case "empty-attribute":
		flags["f"] = &Flag{Key: "f", On: true, Rules: []*Rule{
			rule([]Condition{{Attribute: "", Operator: OpEqual, Values: []string{"x"}}}),
		}}
	case "key-mismatch":
		// Declared key disagrees with the map slot the flag was handed in at.
		flags["f"] = &Flag{Key: "g", On: true, Rules: []*Rule{rule(nil)}}
	case "negative-percentage":
		flags["f"] = &Flag{Key: "f", On: true, Rules: []*Rule{
			{ID: "r", Priority: 1, Percentage: -5, Variants: validVariants},
		}}
	case "valid-condition":
		flags["f"] = &Flag{Key: "f", On: true, Rules: []*Rule{
			rule([]Condition{{Attribute: "tier", Operator: OpIn, Values: []string{"vip"}}}),
		}}
	default:
		panic("unknown parity kind " + kind)
	}
	return &Config{Version: Version2, Flags: flags}
}

// ParseConfig (JSON ingress) and Engine.Install (direct-build ingress) must
// reach the exact same verdict for structurally identical configurations:
// the same rejection sentinel on every malformed case, acceptance on the
// valid one.
func TestDirectInstall_SameStrictnessAsParse(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantErr error
	}{
		{"unknown-operator", `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,
		   "conditions":[{"attribute":"a","operator":"~regex","values":["x"]}],
		   "variants":[{"key":"on","weightBPS":10000}]}]}]}`, ErrUnknownOperator},
		{"in-empty", `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,
		   "conditions":[{"attribute":"a","operator":"in","values":[]}],
		   "variants":[{"key":"on","weightBPS":10000}]}]}]}`, ErrBadCondition},
		{"notin-empty", `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,
		   "conditions":[{"attribute":"a","operator":"not_in","values":[]}],
		   "variants":[{"key":"on","weightBPS":10000}]}]}]}`, ErrBadCondition},
		{"empty-attribute", `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,
		   "conditions":[{"attribute":"","operator":"eq","value":"x"}],
		   "variants":[{"key":"on","weightBPS":10000}]}]}]}`, ErrBadCondition},
		{"negative-percentage", `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,"percentage":-5,
		   "variants":[{"key":"on","weightBPS":10000}]}]}]}`, ErrBadPercentage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, parseErr := ParseConfig([]byte(tc.payload))
			if !errors.Is(parseErr, tc.wantErr) {
				t.Fatalf("ParseConfig verdict: want %v, got %v", tc.wantErr, parseErr)
			}
			if !errors.Is(parseErr, ErrInvalidConfig) {
				t.Fatalf("parse rejection must wrap ErrInvalidConfig, got %v", parseErr)
			}

			e := NewEngine()
			installErr := e.Install(handConfigForParity(tc.name))
			if !errors.Is(installErr, tc.wantErr) {
				t.Fatalf("Install verdict: want %v, got %v", tc.wantErr, installErr)
			}
			if !errors.Is(installErr, ErrInvalidConfig) {
				t.Fatalf("install rejection must wrap ErrInvalidConfig, got %v", installErr)
			}
			// A rejected first install must leave the engine without a config,
			// never serving the malformed one (evaluations report unavailable).
			if got := e.Evaluate(context.Background(), User{ID: "u"}, "f"); got.Status != StatusUnavailable {
				t.Fatalf("rejected config must not be installed: %+v", got)
			}
		})
	}

	// The key-mismatch case has no JSON representation (a decoded flag always
	// lands at its own key), so it is only reachable via the direct ingress.
	t.Run("key-mismatch", func(t *testing.T) {
		e := NewEngine()
		err := e.Install(handConfigForParity("key-mismatch"))
		if !errors.Is(err, ErrFlagKeyMismatch) || !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("want ErrFlagKeyMismatch wrapped, got %v", err)
		}
		if got := e.Evaluate(context.Background(), User{ID: "u"}, "f"); got.Status != StatusUnavailable {
			t.Fatalf("mismatched config must not be installed: %+v", got)
		}
	})

	// A valid hand-built condition rule installs and evaluates the same way
	// as its parsed counterpart.
	t.Run("valid-condition", func(t *testing.T) {
		parsed, err := ParseConfig([]byte(`{"version":"v2","flags":[{"key":"f","on":true,"rules":[
		  {"id":"r","priority":1,
		   "conditions":[{"attribute":"tier","operator":"in","values":["vip"]}],
		   "variants":[{"key":"on","weightBPS":10000}]}]}]}`))
		if err != nil {
			t.Fatalf("parse valid: %v", err)
		}
		e1, e2 := NewEngine(), NewEngine()
		e1.Snapshot(parsed)
		if err := e2.Install(handConfigForParity("valid-condition")); err != nil {
			t.Fatalf("install valid: %v", err)
		}
		vip := User{ID: "u1", Attributes: map[string]string{"tier": "vip"}}
		other := User{ID: "u2", Attributes: map[string]string{"tier": "normal"}}
		for i, e := range []*Engine{e1, e2} {
			if r := e.Evaluate(context.Background(), vip, "f"); !r.On || r.MatchedRule != "r" {
				t.Fatalf("engine %d vip: %+v", i, r)
			}
			if r := e.Evaluate(context.Background(), other, "f"); r.On || r.MatchedRule != "" {
				t.Fatalf("engine %d non-vip: %+v", i, r)
			}
		}
	})
}

// When a running engine already serves a good config, rejecting a malformed
// direct-build config (bad conditions, key mismatch, bad percentage) must
// leave the previous snapshot fully in service; evaluations never come back
// empty or unavailable during the churn.
func TestDirectInstall_RejectionKeepsPreviousConfigServing(t *testing.T) {
	e := NewEngine()
	good := installableConfig(handFlag("base"))
	if err := e.Install(good); err != nil {
		t.Fatalf("seed: %v", err)
	}

	bad := []*Config{
		handConfigForParity("unknown-operator"),
		handConfigForParity("in-empty"),
		handConfigForParity("notin-empty"),
		handConfigForParity("empty-attribute"),
		handConfigForParity("key-mismatch"),
		handConfigForParity("negative-percentage"),
	}
	for i, cfg := range bad {
		err := e.Install(cfg)
		if err == nil {
			t.Fatalf("case %d expected rejection", i)
		}
		r := e.Evaluate(context.Background(), User{ID: "keep-me"}, "base")
		if r.Status == "" || r.Status == StatusUnavailable || !r.On ||
			r.Variant != "on" || r.MatchedRule != "base-r" {
			t.Fatalf("case %d previous config not serving after reject: %+v", i, r)
		}
	}

	// A subsequent valid swap still takes effect.
	next := installableConfig(handFlag("base"), handFlag("extra", "base"))
	if err := e.Install(next); err != nil {
		t.Fatalf("valid swap after rejections: %v", err)
	}
	if r := e.Evaluate(context.Background(), User{ID: "u"}, "extra"); !r.On {
		t.Fatalf("valid swap not serving: %+v", r)
	}
}

// Valid swaps and invalid direct installs (conditions + key mismatch kinds)
// interleaved with concurrent Evaluate calls must never block, panic, or
// return an empty/jittery result. Runs meaningfully under -race.
func TestDirectInstall_ConcurrentSwapRejectAndEvaluate(t *testing.T) {
	e := NewEngine()
	if err := e.Install(installableConfig(handFlag("base"))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	valid := installableConfig(handFlag("base"), handFlag("extra", "base"))
	invalids := []*Config{
		handConfigForParity("unknown-operator"),
		handConfigForParity("in-empty"),
		handConfigForParity("key-mismatch"),
	}

	const workers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan string, workers*4)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				r := e.Evaluate(context.Background(),
					User{ID: "u-" + strconv.Itoa(w) + "-" + strconv.Itoa(i)}, "base")
				if r.Status == "" || r.Status == StatusUnavailable ||
					r.MatchedRule == "" || !r.On {
					select {
					case errs <- "bad result: " + r.Reason:
					default:
					}
					return
				}
			}
		}(w)
	}

	for i := 0; i < 300; i++ {
		if err := e.Install(valid); err != nil {
			t.Fatalf("valid install rejected: %v", err)
		}
		if err := e.Install(invalids[i%len(invalids)]); err == nil {
			t.Fatal("invalid config installed")
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}
	if r := e.Evaluate(context.Background(), User{ID: "u"}, "extra"); !r.On {
		t.Fatalf("final valid config not serving: %+v", r)
	}
}

// v1 flag-level percent outside [0,100] was illegal in the legacy schema too;
// the upgrade path must reject it with ErrBadPercentage instead of silently
// treating it as the omitted default (which would mean full rollout).
func TestV1LegacyPercent_OutOfRangeRejected(t *testing.T) {
	for _, pct := range []string{"-1", "-100", "101", "250"} {
		payload := `{"version":"v1","flags":[{"key":"old","on":true,"percent":` + pct + `}]}`
		_, err := ParseConfig([]byte(payload))
		if !errors.Is(err, ErrBadPercentage) || !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("percent=%s: want ErrBadPercentage wrapped, got %v", pct, err)
		}
	}

	// Rejected upgrade must not displace the last good snapshot.
	e := NewEngine()
	if err := e.Install(installableConfig(handFlag("base"))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bad, err := ParseConfig([]byte(`{"version":"v1","flags":[{"key":"old","on":true,"percent":-5}]}`))
	if err == nil {
		t.Fatalf("v1 negative percent must not parse, got %+v", bad)
	}
	if r := e.Evaluate(context.Background(), User{ID: "u"}, "base"); !r.On {
		t.Fatalf("rejected v1 upgrade displaced previous config: %+v", r)
	}
}

// Legal v1 percent values must survive the upgrade unchanged: percent:0
// stays legacy full-rollout (migrated to omitted percentage), percent:50
// migrates to an explicit 50 rule. Explicit 0 vs omitted in native v2
// remains distinct (covered by TestExplicitZeroPercentage_*).
func TestV1LegacyPercent_LegalValuesPreserved(t *testing.T) {
	zero, err := ParseConfig([]byte(`{"version":"v1","flags":[{"key":"z","on":true,"percent":0}]}`))
	if err != nil {
		t.Fatalf("v1 percent 0 rejected: %v", err)
	}
	r0 := zero.Flags["z"].Rules[0]
	if r0.Percentage != 100 {
		t.Fatalf("v1 percent 0 must migrate as omitted (=100), got %v", r0.Percentage)
	}

	half, err := ParseConfig([]byte(`{"version":"v1","flags":[{"key":"h","on":true,"percent":50}]}`))
	if err != nil {
		t.Fatalf("v1 percent 50 rejected: %v", err)
	}
	if rh := half.Flags["h"].Rules[0]; rh.Percentage != 50 || rh.ID != "h-legacy-default" {
		t.Fatalf("v1 percent 50 not preserved: %+v", rh)
	}
}
