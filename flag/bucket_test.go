package flag

import (
	"context"
	"testing"
)

// rolloutConfig is parameterised by rule listing order so we can prove that
// reordering rules cannot change a user's variant.
func rolloutConfig(rulesInOrder ...string) string {
	const tmpl = `{
	  "version": "v2",
	  "flags": [
	    {
	      "key": "exp",
	      "on": true,
	      "rules": [
%s
	      ]
	    }
	  ]
	}`
	const ruleA = `        {"id": "a", "priority": 1, "conditions": [{"attribute": "seg", "operator": "in", "values": ["A"]}],
	         "percentage": 100, "variants": [{"key": "va", "weightBPS": 10000}]}`
	const ruleB = `        {"id": "b", "priority": 2, "conditions": [{"attribute": "seg", "operator": "in", "values": ["B"]}],
	         "percentage": 100, "variants": [{"key": "vb", "weightBPS": 10000}]}`
	const ruleC = `        {"id": "c", "priority": 3,
	         "percentage": 100, "variants": [
	           {"key": "c1", "weightBPS": 4000},
	           {"key": "c2", "weightBPS": 3500},
	           {"key": "c3", "weightBPS": 2500}
	         ]}`
	pick := map[string]string{"a": ruleA, "b": ruleB, "c": ruleC}
	var body string
	for i, id := range rulesInOrder {
		if i > 0 {
			body += ",\n"
		}
		body += pick[id]
	}
	return formatJSON(tmpl, body)
}

func formatJSON(tmpl, body string) string {
	// simple placeholder replacement avoiding fmt in test templates
	out := ""
	for i := 0; i < len(tmpl); i++ {
		if i+1 < len(tmpl) && tmpl[i] == '%' && tmpl[i+1] == 's' {
			out += body
			i++
			continue
		}
		out += string(tmpl[i])
	}
	return out
}

func evalVariant(t *testing.T, payload, userID, seg string) Result {
	t.Helper()
	cfg, err := ParseConfig([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := NewEngine()
	e.Snapshot(cfg)
	return e.Evaluate(context.Background(), User{ID: userID, Attributes: map[string]string{"seg": seg}}, "exp")
}

func TestRolloutValue_Deterministic(t *testing.T) {
	in := RolloutInput{FlagKey: "exp", UserID: "user-42"}
	first := RolloutValue(in)
	for i := 0; i < 100; i++ {
		if RolloutValue(in) != first {
			t.Fatal("rollout value changed across calls")
		}
	}
	if first < 0 || first >= 10000 {
		t.Fatalf("bucket out of range: %d", first)
	}
}

func TestSameUserSameBucket_AnyEvaluationOrder(t *testing.T) {
	payload := rolloutConfig("a", "b", "c")
	cfg, err := ParseConfig([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := NewEngine()
	e.Snapshot(cfg)
	u := User{ID: "stable-user-7", Attributes: map[string]string{"seg": "C"}}

	want := e.Evaluate(context.Background(), u, "exp")
	// Evaluate in arbitrary interleavings / repeated times.
	for i := 0; i < 50; i++ {
		got := e.Evaluate(context.Background(), u, "exp")
		if got.Variant != want.Variant || got.On != want.On || got.MatchedRule != want.MatchedRule {
			t.Fatalf("iteration %d drift: want variant=%s got %+v", i, want.Variant, got)
		}
	}
}

func TestSameUserSameBucket_RuleReorder(t *testing.T) {
	orders := [][]string{
		{"a", "b", "c"},
		{"c", "b", "a"},
		{"b", "a", "c"},
		{"c", "a", "b"},
	}
	var want string
	for i, ord := range orders {
		got := evalVariant(t, rolloutConfig(ord...), "reorder-user-9", "C")
		if i == 0 {
			want = got.Variant
		} else if got.Variant != want {
			t.Fatalf("order %v produced variant %s, want %s", ord, got.Variant, want)
		}
		if got.MatchedRule != "c" {
			t.Fatalf("expected catch-all rule c to match, got %s", got.MatchedRule)
		}
	}
}

func TestPriority_LowerNumberWins(t *testing.T) {
	// User matches both a and c; rule a (priority 1) must win regardless
	// of listing order.
	payloads := []string{rolloutConfig("a", "b", "c"), rolloutConfig("c", "a")}
	for _, p := range payloads {
		got := evalVariant(t, p, "prio-user", "A")
		if got.MatchedRule != "a" || got.Variant != "va" {
			t.Fatalf("priority result: %+v", got)
		}
	}
}

func TestRolloutDistribution_RoughlyProportional(t *testing.T) {
	// Statistical sanity check: 100% rollout with 50/50 variants over a
	// large synthetic population should be close to balanced.
	payload := `{
	  "version": "v2",
	  "flags": [{"key": "exp", "on": true, "rules": [
	    {"id": "r", "priority": 1, "percentage": 100,
	     "variants": [{"key": "x", "weightBPS": 5000}, {"key": "y", "weightBPS": 5000}]}
	  ]}]
	}`
	cfg, _ := ParseConfig([]byte(payload))
	e := NewEngine()
	e.Snapshot(cfg)
	counts := map[string]int{}
	const n = 5000
	for i := 0; i < n; i++ {
		r := e.Evaluate(context.Background(), User{ID: "user-" + itoaTest(i)}, "exp")
		counts[r.Variant]++
	}
	// allow generous tolerance: hash-based, but 5000 samples at 50% is tight.
	lo, hi := n/2-n/10, n/2+n/10
	if counts["x"] < lo || counts["x"] > hi {
		t.Fatalf("distribution skewed: %+v", counts)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
