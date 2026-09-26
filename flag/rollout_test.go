package flag

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestVariants_AllReachableAndWeightedAt50k: within a partial rollout with
// multiple weighted variants, every declared variant must be reachable by
// rolled-in users and appear in proportion to its declared weight over a
// 50k-user population.
func TestVariants_AllReachableAndWeightedAt50k(t *testing.T) {
	const pct = 30
	payload := `{
	  "version": "v2",
	  "flags": [{"key": "exp", "on": true, "rules": [
	    {"id": "r", "priority": 1, "percentage": 30,
	     "variants": [
	       {"key": "a", "weightBPS": 5000},
	       {"key": "b", "weightBPS": 3000},
	       {"key": "c", "weightBPS": 2000}
	     ]}
	  ]}]
	}`
	cfg, err := ParseConfig([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := NewEngine()
	if err := e.Install(cfg); err != nil {
		t.Fatalf("install: %v", err)
	}

	const n = 50000
	counts := map[string]int{}
	on := 0
	ctx := context.Background()
	for i := 0; i < n; i++ {
		r := e.Evaluate(ctx, User{ID: "user-" + itoaTest(i)}, "exp")
		if r.On {
			on++
			counts[r.Variant]++
		}
	}

	// Every declared variant must be covered.
	for _, key := range []string{"a", "b", "c"} {
		if counts[key] == 0 {
			t.Fatalf("variant %q never served: counts=%+v", key, counts)
		}
	}

	// Overall rollout fraction must be close to 30%.
	wantOn := n * pct / 100
	if !withinFraction(on, wantOn, 0.05) {
		t.Fatalf("rollout size %d, want ~%d", on, wantOn)
	}

	// Among rolled-in users, each variant must follow its declared weight
	// (50/30/20) within a 10% relative tolerance.
	weights := map[string]float64{"a": 0.5, "b": 0.3, "c": 0.2}
	for key, w := range weights {
		want := int(float64(on) * w)
		if !withinFraction(counts[key], want, 0.10) {
			t.Fatalf("variant %q count %d, want ~%d (on=%d, counts=%+v)", key, counts[key], want, on, counts)
		}
	}
}

// withinFraction reports whether got is within rel relative error of want.
func withinFraction(got, want int, rel float64) bool {
	if want == 0 {
		return got == 0
	}
	d := got - want
	if d < 0 {
		d = -d
	}
	return float64(d) <= rel*float64(want)
}

// multiVariantRule builds a catch-all v2 rule at the given percentage with
// three weighted variants.
func multiVariantRule(ruleID string, pct int) string {
	return `{"id": "` + ruleID + `", "priority": 1, "percentage": ` + itoaTest(pct) + `,
	     "variants": [
	       {"key": "a", "weightBPS": 5000},
	       {"key": "b", "weightBPS": 3000},
	       {"key": "c", "weightBPS": 2000}
	     ]}`
}

func rolloutEngineAt(t *testing.T, pct int) *Engine {
	t.Helper()
	body := `{"version":"v2","flags":[{"key":"exp","on":true,"rules":[` +
		multiVariantRule("r", pct) + `]}]}`
	cfg, err := ParseConfig([]byte(body))
	if err != nil {
		t.Fatalf("parse pct=%d: %v", pct, err)
	}
	e := NewEngine()
	if err := e.Install(cfg); err != nil {
		t.Fatalf("install pct=%d: %v", pct, err)
	}
	return e
}

// TestRollout_MonotoneAndVariantSticky_10to50: as the percentage is raised
// 10 -> 20 -> 30 -> 40 -> 50, admitted users are never recalled and keep
// exactly the same variant; only additional users get admitted.
func TestRollout_MonotoneAndVariantSticky_10to50(t *testing.T) {
	const n = 50000
	pcts := []int{10, 20, 30, 40, 50}
	engines := make(map[int]*Engine, len(pcts))
	for _, p := range pcts {
		engines[p] = rolloutEngineAt(t, p)
	}
	ctx := context.Background()

	prevAdmitted := map[string]string{} // userID -> variant at previous step
	prevSize := 0
	for _, p := range pcts {
		e := engines[p]
		admitted := map[string]string{}
		for i := 0; i < n; i++ {
			uid := "user-" + itoaTest(i)
			r := e.Evaluate(ctx, User{ID: uid}, "exp")
			if r.On {
				admitted[uid] = r.Variant
			}
		}
		if len(admitted) < prevSize {
			t.Fatalf("rollout shrank at pct=%d: %d -> %d", p, prevSize, len(admitted))
		}
		for uid, variant := range prevAdmitted {
			newVariant, still := admitted[uid]
			if !still {
				t.Fatalf("user %s recalled while raising to %d%%", uid, p)
			}
			if newVariant != variant {
				t.Fatalf("user %s variant changed %s -> %s at pct=%d", uid, variant, newVariant, p)
			}
		}
		prevAdmitted = admitted
		prevSize = len(admitted)
	}
}

// TestExplicitZero_VersusOmitted: "percentage": 0 means "matched but never
// served" while an omitted percentage means the documented 100% default. The
// matched rule and reason must still be visible in the zero case.
func TestExplicitZero_VersusOmitted(t *testing.T) {
	ctx := context.Background()

	zero := `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
	  {"id":"rz","priority":1,"percentage":0,
	   "variants":[{"key":"a","weightBPS":5000},{"key":"b","weightBPS":5000}]}]}]}`
	cfg, err := ParseConfig([]byte(zero))
	if err != nil {
		t.Fatalf("parse explicit-zero: %v", err)
	}
	rz := cfg.Flags["f"].Rules[0]
	if rz.Percentage != 0 || !rz.PercentageExplicit {
		t.Fatalf("explicit zero normalized wrong: %+v", rz)
	}
	e := NewEngine()
	if err := e.Install(cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	for i := 0; i < 1000; i++ {
		r := e.Evaluate(ctx, User{ID: "u-" + itoaTest(i)}, "f")
		if r.On || r.Variant != "" {
			t.Fatalf("explicit 0%% served user %d: %+v", i, r)
		}
		if r.Source != SourceRuleZeroRollout || r.MatchedRule != "rz" {
			t.Fatalf("explicit 0%% must keep matched rule/source: %+v", r)
		}
	}

	omitted := `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
	  {"id":"rd","priority":1,
	   "variants":[{"key":"a","weightBPS":5000},{"key":"b","weightBPS":5000}]}]}]}`
	cfg2, err := ParseConfig([]byte(omitted))
	if err != nil {
		t.Fatalf("parse omitted: %v", err)
	}
	rd := cfg2.Flags["f"].Rules[0]
	if rd.Percentage != 100 || rd.PercentageExplicit {
		t.Fatalf("omitted percentage normalized wrong: %+v", rd)
	}
	e2 := NewEngine()
	if err := e2.Install(cfg2); err != nil {
		t.Fatalf("install: %v", err)
	}
	on := 0
	for i := 0; i < 1000; i++ {
		if r := e2.Evaluate(ctx, User{ID: "u-" + itoaTest(i)}, "f"); r.On {
			on++
		}
	}
	if on != 1000 {
		t.Fatalf("omitted percentage must mean full rollout, only %d/1000 on", on)
	}
}

// TestExplicitZero_AllIngressesConsistent: file/push/refresh all parse via
// the same boundary; the zero-vs-omitted distinction survives each one.
func TestExplicitZero_AllIngressesConsistent(t *testing.T) {
	payload := []byte(`{"version":"v2","flags":[{"key":"f","on":true,"rules":[
	  {"id":"rz","priority":1,"percentage":0,
	   "variants":[{"key":"a","weightBPS":10000}]}]}]}`)

	// 1) parsing ingress (file / direct push / refresh share ParseConfig)
	cfg, err := ParseConfig(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := NewEngine()
	// 2) direct install ingress
	if err := e.Install(cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	// 3) background refresh ingress
	ref := NewRefresher(e, LoaderFunc(func(context.Context) ([]byte, error) {
		return payload, nil
	}))
	defer ref.Close()
	if err := ref.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	r := e.Evaluate(context.Background(), User{ID: "u1"}, "f")
	if r.On || r.Source != SourceRuleZeroRollout || r.MatchedRule != "rz" {
		t.Fatalf("zero semantics not consistent across ingresses: %+v", r)
	}
}

// TestV1_LegacyZeroPercent_StaysFullRollout: v1 had no explicit-zero
// semantics at the flag level; a v1 percent of 0 used to mean the omitted
// default (full rollout). Upgrading must not change that.
func TestV1_LegacyZeroPercent_StaysFullRollout(t *testing.T) {
	payload := []byte(`{"version":"v1","flags":[{"key":"old","on":true,"percent":0}]}`)
	cfg, err := ParseConfig(payload)
	if err != nil {
		t.Fatalf("v1 parse: %v", err)
	}
	r0 := cfg.Flags["old"].Rules[0]
	if r0.Percentage != 100 || r0.PercentageExplicit {
		t.Fatalf("v1 percent=0 must migrate as omitted default, got %+v", r0)
	}
	e := NewEngine()
	if err := e.Install(cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	for i := 0; i < 200; i++ {
		if r := e.Evaluate(context.Background(), User{ID: "u-" + itoaTest(i)}, "old"); !r.On {
			t.Fatalf("legacy v1 percent=0 must stay full rollout: %+v", r)
		}
	}
}

func validRule(id string) *Rule {
	return &Rule{ID: id, Priority: 1, Percentage: 100,
		Variants: []Variant{{Key: "on", WeightBPS: 10000}}}
}

// TestInstall_RejectsInvalidHandBuiltConfigs: callers that bypass parsing
// still face the same strict boundary at Install.
func TestInstall_RejectsInvalidHandBuiltConfigs(t *testing.T) {
	base := &Config{Version: Version2, Flags: map[string]*Flag{
		"base": {Key: "base", On: true, Rules: []*Rule{validRule("base-r")}},
	}}
	e := NewEngine()
	if err := e.Install(base); err != nil {
		t.Fatalf("install base: %v", err)
	}

	cases := map[string]struct {
		cfg  *Config
		want error
	}{
		"nil config":               {nil, ErrEmptyConfig},
		"empty flags":              {&Config{Version: Version2, Flags: map[string]*Flag{}}, ErrEmptyConfig},
		"unknown version":          {&Config{Version: "v9", Flags: map[string]*Flag{"a": {Key: "a", On: true, Rules: []*Rule{validRule("r")}}}}, ErrUnknownVersion},
		"self loop":                {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Prerequisites: []string{"a"}, Rules: []*Rule{validRule("r")}}}}, ErrCircularDependency},
		"two-node cycle":           {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Prerequisites: []string{"b"}, Rules: []*Rule{validRule("ar")}}, "b": {Key: "b", On: true, Prerequisites: []string{"a"}, Rules: []*Rule{validRule("br")}}}}, ErrCircularDependency},
		"missing dependency":       {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Prerequisites: []string{"ghost"}, Rules: []*Rule{validRule("r")}}}}, ErrMissingDependency},
		"contradiction off prereq": {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Prerequisites: []string{"b"}, Rules: []*Rule{validRule("ar")}}, "b": {Key: "b", On: false}}}, ErrDependencyContradiction},
		"duplicate prerequisite":   {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Prerequisites: []string{"b", "b"}, Rules: []*Rule{validRule("ar")}}, "b": {Key: "b", On: true, Rules: []*Rule{validRule("br")}}}}, ErrDependencyContradiction},
		"non-positive priority":    {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Rules: []*Rule{{ID: "r", Priority: 0, Percentage: 100, Variants: []Variant{{Key: "on", WeightBPS: 10000}}}}}}}, ErrBadPriority},
		"percentage out of range":  {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 120, Variants: []Variant{{Key: "on", WeightBPS: 10000}}}}}}}, ErrBadPercentage},
		"weights do not sum":       {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100, Variants: []Variant{{Key: "x", WeightBPS: 6000}}}}}}}, ErrWeightSum},
		"empty variants":           {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100}}}}}, ErrEmptyVariants},
		"duplicate rule id":        {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Rules: []*Rule{validRule("r"), validRule("r")}}}}, ErrDuplicateRuleID},
		"unknown operator":         {&Config{Version: Version2, Flags: map[string]*Flag{"a": {Key: "a", On: true, Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100, Variants: []Variant{{Key: "on", WeightBPS: 10000}}, Conditions: []Condition{{Attribute: "seg", Operator: "~regex"}}}}}}}, ErrUnknownOperator},
		"map key mismatch":         {&Config{Version: Version2, Flags: map[string]*Flag{"other": {Key: "a", On: true, Rules: []*Rule{validRule("r")}}}}, ErrMissingField},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := e.Install(tc.cfg)
			if err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s: want sentinel %v, got %v", name, tc.want, err)
			}
		})
	}

	// Old config must still be serving after every rejection.
	if e.Current() != Version2 {
		t.Fatalf("active version changed after rejections: %q", e.Current())
	}
	r := e.Evaluate(context.Background(), User{ID: "u1"}, "base")
	if !r.On || r.MatchedRule != "base-r" {
		t.Fatalf("old config no longer serving: %+v", r)
	}
}

// TestConcurrent_EvaluationAcrossSwaps: many evaluators run while the engine
// alternates between valid configs (successful swaps) and invalid configs
// (rejected, last-good kept). No evaluator may panic, block, observe an
// empty result, or get an answer inconsistent with one of the two snapshots.
func TestConcurrent_EvaluationAcrossSwaps(t *testing.T) {
	good1 := buildConfig(t,
		map[string]interface{}{
			"key": "base", "on": true,
			"rules": []map[string]interface{}{{
				"id": "r", "priority": 1, "percentage": 100,
				"variants": []map[string]interface{}{
					{"key": "v1a", "weightBPS": 5000},
					{"key": "v1b", "weightBPS": 5000},
				},
			}},
		})
	good2 := buildConfig(t,
		map[string]interface{}{
			"key": "base", "on": true,
			"rules": []map[string]interface{}{{
				"id": "r", "priority": 1, "percentage": 50,
				"variants": []map[string]interface{}{
					{"key": "v2a", "weightBPS": 3000},
					{"key": "v2b", "weightBPS": 7000},
				},
			}},
		})
	bad := buildConfig(t, onFlag("base", "missing-flag"))
	if _, err := ParseConfig(bad); err == nil {
		t.Fatal("bad payload unexpectedly parsed")
	}
	cfg1, err := ParseConfig(good1)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine()
	if err := e.Install(cfg1); err != nil {
		t.Fatal(err)
	}
	cfg2, err := ParseConfig(good2)
	if err != nil {
		t.Fatal(err)
	}
	invalid := &Config{Version: Version2, Flags: map[string]*Flag{
		"base": {Key: "base", On: true, Prerequisites: []string{"missing-flag"},
			Rules: []*Rule{validRule("r")}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	const evaluators = 16
	const perEval = 2000
	for g := 0; g < evaluators; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perEval; i++ {
				r := e.Evaluate(ctx, User{ID: "user-" + itoaTest(i)}, "base")
				if r.Status == "" || r.Source == "" || r.MatchedRule != "r" {
					t.Errorf("empty/inconsistent result during swaps: %+v", r)
					return
				}
				switch {
				case r.On:
					// Variant must belong to whichever snapshot was observed.
					switch r.Variant {
					case "v1a", "v1b", "v2a", "v2b":
					default:
						t.Errorf("on result with variant from no snapshot: %q", r.Variant)
						return
					}
				case r.Status == StatusOff && r.Source == SourceDefault && r.Variant == "":
					// Legitimate rollout-miss against the 50% snapshot.
				default:
					t.Errorf("inconsistent off result during swaps: %+v", r)
					return
				}
			}
		}(g)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			switch i % 3 {
			case 0:
				_ = e.Install(cfg1)
			case 1:
				_ = e.Install(cfg2)
			case 2:
				if err := e.Install(invalid); err == nil {
					t.Error("invalid config installed")
				}
			}
		}
	}()

	wg.Wait()

	// After all the noise the last-good semantics hold: flag still evaluates.
	r := e.Evaluate(context.Background(), User{ID: "u1"}, "base")
	if r.Status != StatusOn && r.Status != StatusOff {
		t.Fatalf("post-swap evaluation broken: %+v", r)
	}
}

// TestInstall_RejectedSwapUnderConcurrentLoad_KeepsServing is a focused
// version where only rejections happen: the first config must remain the
// single source of truth for every concurrent evaluation.
func TestInstall_RejectedSwapUnderConcurrentLoad_KeepsServing(t *testing.T) {
	cfg, err := ParseConfig(buildConfig(t, onFlag("base")))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine()
	if err := e.Install(cfg); err != nil {
		t.Fatal(err)
	}
	bad := &Config{Version: Version2, Flags: map[string]*Flag{
		"base": {Key: "base", On: true, Prerequisites: []string{"base"},
			Rules: []*Rule{validRule("base-r")}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				r := e.Evaluate(ctx, User{ID: "u" + itoaTest(i%97)}, "base")
				if !r.On || r.MatchedRule != "base-r" || r.Variant != "on" {
					t.Errorf("evaluation drifted while rejecting swaps: %+v", r)
					return
				}
			}
		}()
	}
	for i := 0; i < 500; i++ {
		if err := e.Install(bad); err == nil {
			t.Fatal("self-loop config installed")
		}
	}
	wg.Wait()
}

// TestStability_AcrossRestartAndRuleReorder: a fresh engine (process
// restart) and a payload with the rules physically reordered must give the
// same eligibility and the same variant for every user.
func TestStability_AcrossRestartAndRuleReorder(t *testing.T) {
	rule := func(id string, pct int) string {
		return `{"id":"` + id + `","priority":1,"percentage":` + itoaTest(pct) + `,
		  "variants":[
		    {"key":"a","weightBPS":4000},
		    {"key":"b","weightBPS":3500},
		    {"key":"c","weightBPS":2500}]}`
	}
	mk := func(body string) *Engine {
		cfg, err := ParseConfig([]byte(`{"version":"v2","flags":[{"key":"exp","on":true,"rules":[` + body + `]}]}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		e := NewEngine()
		if err := e.Install(cfg); err != nil {
			t.Fatalf("install: %v", err)
		}
		return e
	}
	// A reordering-only config: add a non-matching higher-priority rule
	// listed before/after the rolling rule across two payloads.
	guard := `{"id":"guard","priority":2,
	  "conditions":[{"attribute":"seg","operator":"in","values":["none"]}],
	  "percentage":100,"variants":[{"key":"g","weightBPS":10000}]}`
	e1 := mk(rule("main", 40) + "," + guard)
	e2 := mk(guard + "," + rule("main", 40)) // physically reordered
	e3 := mk(rule("main", 40) + "," + guard) // fresh engine, same payload (restart)

	ctx := context.Background()
	for i := 0; i < 5000; i++ {
		u := User{ID: "stable-" + itoaTest(i)}
		r1 := e1.Evaluate(ctx, u, "exp")
		r2 := e2.Evaluate(ctx, u, "exp")
		r3 := e3.Evaluate(ctx, u, "exp")
		if r1.On != r2.On || r1.On != r3.On {
			t.Fatalf("user %d eligibility drift: %v %v %v", i, r1.On, r2.On, r3.On)
		}
		if r1.MatchedRule != "main" || r2.MatchedRule != "main" || r3.MatchedRule != "main" {
			t.Fatalf("matched rule drifted: %s %s %s", r1.MatchedRule, r2.MatchedRule, r3.MatchedRule)
		}
		if r1.On && (r1.Variant != r2.Variant || r1.Variant != r3.Variant) {
			t.Fatalf("user %d variant drift across restart/reorder: %s %s %s", i, r1.Variant, r2.Variant, r3.Variant)
		}
	}
}

// TestEvaluationPerformance_NoRegress: 50k evaluations must stay comfortably
// fast; the second independent hash must not cause a material slowdown
// (bound set generously, ~10x the observed cost).
func TestEvaluationPerformance_NoRegress(t *testing.T) {
	if testing.Short() {
		t.Skip("performance check skipped in -short mode")
	}
	cfg, err := ParseConfig([]byte(`{"version":"v2","flags":[{"key":"exp","on":true,"rules":[
	  {"id":"r","priority":1,"percentage":40,
	   "variants":[
	     {"key":"a","weightBPS":4000},
	     {"key":"b","weightBPS":3500},
	     {"key":"c","weightBPS":2500}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine()
	if err := e.Install(cfg); err != nil {
		t.Fatal(err)
	}
	const n = 50000
	ctx := context.Background()
	start := time.Now()
	var sink int
	for i := 0; i < n; i++ {
		r := e.Evaluate(ctx, User{ID: "user-" + itoaTest(i)}, "exp")
		sink += len(r.Variant)
	}
	elapsed := time.Since(start)
	if sink == 0 { // prevent optimizing away
		t.Fatal("no variants served")
	}
	avg := elapsed / n
	t.Logf("50k evaluations in %s (avg %s/eval)", elapsed, avg)
	// Two sha256 hashes plus sorting still leaves ample headroom under 100µs.
	if avg > 100*time.Microsecond {
		t.Fatalf("evaluation regressed: avg %s per call", avg)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("50k evaluations took %s", elapsed)
	}
}

// BenchmarkEvaluate tracks per-evaluation cost in CI bench runs.
func BenchmarkEvaluate(b *testing.B) {
	cfg, err := ParseConfig([]byte(`{"version":"v2","flags":[{"key":"exp","on":true,"rules":[
	  {"id":"r","priority":1,"percentage":40,
	   "variants":[
	     {"key":"a","weightBPS":4000},
	     {"key":"b","weightBPS":3500},
	     {"key":"c","weightBPS":2500}]}]}]}`))
	if err != nil {
		b.Fatal(err)
	}
	e := NewEngine()
	if err := e.Install(cfg); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.Evaluate(ctx, User{ID: "user-" + itoaTest(i%50000)}, "exp")
	}
}
