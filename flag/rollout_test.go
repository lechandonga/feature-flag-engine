package flag

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rolloutOnlyConfig builds a v2 payload with a single catch-all rule at the
// given percentage and the given variant weights (basis points).
func rolloutOnlyConfig(pct string, variants string) string {
	return `{"version":"v2","flags":[{"key":"exp","on":true,"rules":[
	  {"id":"r","priority":1,"percentage":` + pct + `,"variants":[` + variants + `]}]}]}`
}

const threeVariants = `{"key":"a","weightBPS":4000},{"key":"b","weightBPS":3500},{"key":"c","weightBPS":2500}`

func evalN(t *testing.T, e *Engine, n int) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		r := e.Evaluate(context.Background(), User{ID: "u-" + strconv.Itoa(i)}, "exp")
		if r.Status == "" {
			t.Fatalf("empty status for user %d", i)
		}
		if r.On {
			counts[r.Variant]++
		} else {
			counts["<off>"]++
		}
	}
	return counts
}

// All variants must be reachable inside a partial rollout, in proportions
// matching their declared weights, over a 50k-user population.
func TestPartialRollout_AllVariantsReachableWithWeights(t *testing.T) {
	cfg, err := ParseConfig([]byte(rolloutOnlyConfig("50", threeVariants)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := NewEngine()
	e.Snapshot(cfg)

	const n = 50000
	start := time.Now()
	counts := evalN(t, e, n)
	elapsed := time.Since(start)
	t.Logf("50k evaluations took %s (%s/eval)", elapsed, elapsed/n)

	on := n - counts["<off>"]
	// Rollout ratio should be near 50%.
	if on < n*45/100 || on > n*55/100 {
		t.Fatalf("rollout ratio off: %d/%d on", on, n)
	}
	// Every variant must appear, near its declared share of rolled-out users.
	for _, tc := range []struct {
		key   string
		share float64
	}{
		{"a", 0.40}, {"b", 0.35}, {"c", 0.25},
	} {
		got := counts[tc.key]
		if got == 0 {
			t.Fatalf("variant %s never selected: %+v", tc.key, counts)
		}
		lo := int(float64(on) * (tc.share - 0.03))
		hi := int(float64(on) * (tc.share + 0.03))
		if got < lo || got > hi {
			t.Fatalf("variant %s count %d outside [%d,%d]: %+v", tc.key, got, lo, hi, counts)
		}
	}
	// Performance sanity: no obvious evaluation slowdown (generous bound).
	if elapsed > 10*time.Second {
		t.Fatalf("evaluation too slow: %s for %d evals", elapsed, n)
	}
}

// Raising the rollout percentage must be monotone (nobody rolled back) and
// sticky (nobody's variant changes).
func TestRolloutIncrease_MonotoneAndSticky(t *testing.T) {
	parse := func(pct string) *Engine {
		cfg, err := ParseConfig([]byte(rolloutOnlyConfig(pct, threeVariants)))
		if err != nil {
			t.Fatalf("parse %s: %v", pct, err)
		}
		e := NewEngine()
		e.Snapshot(cfg)
		return e
	}
	e10, e30, e50 := parse("10"), parse("30"), parse("50")

	type obs struct {
		on      bool
		variant string
	}
	evalAll := func(e *Engine) map[string]obs {
		out := map[string]obs{}
		for i := 0; i < 20000; i++ {
			id := "u-" + strconv.Itoa(i)
			r := e.Evaluate(context.Background(), User{ID: id}, "exp")
			out[id] = obs{on: r.On, variant: r.Variant}
		}
		return out
	}
	at10, at30, at50 := evalAll(e10), evalAll(e30), evalAll(e50)

	grew := 0
	for id, o10 := range at10 {
		o30, o50 := at30[id], at50[id]
		if o10.on {
			if !o30.on || !o50.on {
				t.Fatalf("user %s rolled back: 10%%=%+v 30%%=%+v 50%%=%+v", id, o10, o30, o50)
			}
			if o10.variant != o30.variant || o10.variant != o50.variant {
				t.Fatalf("user %s variant changed: 10%%=%s 30%%=%s 50%%=%s",
					id, o10.variant, o30.variant, o50.variant)
			}
		}
		if o30.on {
			if !o50.on || o30.variant != o50.variant {
				t.Fatalf("user %s not monotone/sticky between 30%% and 50%%: %+v vs %+v", id, o30, o50)
			}
		}
		if !o10.on && o50.on {
			grew++
		}
	}
	if grew == 0 {
		t.Fatal("raising the percentage admitted no new users; rollout is not expanding")
	}
}

// An explicit 0% rollout means "matched, serve nobody" while still reporting
// the matched rule; an omitted percentage keeps the legacy 100% default.
func TestExplicitZeroPercentage_VersusOmitted(t *testing.T) {
	zero := `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
	  {"id":"r0","priority":1,"percentage":0,"variants":[{"key":"a","weightBPS":10000}]}]}]}`
	omitted := `{"version":"v2","flags":[{"key":"f","on":true,"rules":[
	  {"id":"r1","priority":1,"variants":[{"key":"a","weightBPS":10000}]}]}]}`

	cfg0, err := ParseConfig([]byte(zero))
	if err != nil {
		t.Fatalf("parse explicit 0: %v", err)
	}
	if cfg0.Flags["f"].Rules[0].Percentage != 0 {
		t.Fatalf("explicit 0 not preserved: %+v", cfg0.Flags["f"].Rules[0])
	}
	e0 := NewEngine()
	e0.Snapshot(cfg0)
	r := e0.Evaluate(context.Background(), User{ID: "anyone"}, "f")
	if r.On || r.Status != StatusOff {
		t.Fatalf("explicit 0 must serve nobody: %+v", r)
	}
	if r.MatchedRule != "r0" {
		t.Fatalf("matched rule must stay visible: %+v", r)
	}
	if !strings.Contains(r.Reason, "explicitly 0") {
		t.Fatalf("reason should explain explicit 0: %q", r.Reason)
	}

	cfg1, err := ParseConfig([]byte(omitted))
	if err != nil {
		t.Fatalf("parse omitted: %v", err)
	}
	if cfg1.Flags["f"].Rules[0].Percentage != 100 {
		t.Fatalf("omitted percentage must default to 100: %+v", cfg1.Flags["f"].Rules[0])
	}
	e1 := NewEngine()
	e1.Snapshot(cfg1)
	r2 := e1.Evaluate(context.Background(), User{ID: "anyone"}, "f")
	if !r2.On || r2.Variant != "a" {
		t.Fatalf("omitted percentage must roll out fully: %+v", r2)
	}
}

// Explicit 0 must behave identically across every ingress: local file parse,
// direct push parse and background refresh all funnel through ParseConfig;
// the refresher must also install an explicit-0 config as-is.
func TestExplicitZeroPercentage_ConsistentAcrossIngresses(t *testing.T) {
	payload := []byte(`{"version":"v2","flags":[{"key":"f","on":true,"rules":[
	  {"id":"r0","priority":1,"percentage":0,"variants":[{"key":"a","weightBPS":10000}]}]}]}`)

	// Ingress 1: direct parse (local file / push share ParseConfig).
	cfg, err := ParseConfig(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e1 := NewEngine()
	e1.Snapshot(cfg)

	// Ingress 2: background refresh.
	e2 := NewEngine()
	r := NewRefresher(e2, LoaderFunc(func(context.Context) ([]byte, error) { return payload, nil }))
	if err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	for i, e := range []*Engine{e1, e2} {
		got := e.Evaluate(context.Background(), User{ID: "u"}, "f")
		if got.On || got.MatchedRule != "r0" {
			t.Fatalf("ingress %d diverged: %+v", i, got)
		}
	}
}

// v1 legacy percent:0 historically meant "full rollout"; upgrading must not
// change that behaviour.
func TestV1LegacyPercentZero_KeepsLegacySemantics(t *testing.T) {
	payload := `{"version":"v1","flags":[{"key":"old","on":true,"percent":0}]}`
	cfg, err := ParseConfig([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := NewEngine()
	e.Snapshot(cfg)
	r := e.Evaluate(context.Background(), User{ID: "u"}, "old")
	if !r.On {
		t.Fatalf("v1 percent:0 must keep legacy full-rollout semantics: %+v", r)
	}
}
