package flag

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
)

func handFlag(key string, prereqs ...string) *Flag {
	return &Flag{
		Key: key, On: true, Prerequisites: prereqs,
		Rules: []*Rule{{
			ID: key + "-r", Priority: 1, Percentage: 100,
			Variants: []Variant{{Key: "on", WeightBPS: 10000}},
		}},
	}
}

func installableConfig(flags ...*Flag) *Config {
	c := &Config{Version: Version2, Flags: map[string]*Flag{}}
	for _, f := range flags {
		c.Flags[f.Key] = f
	}
	return c
}

// Install must reject every dependency problem kind and keep the previous
// configuration serving.
func TestInstall_RejectsInvalidAndKeepsServing(t *testing.T) {
	e := NewEngine()
	good := installableConfig(handFlag("base"))
	if err := e.Install(good); err != nil {
		t.Fatalf("install good: %v", err)
	}

	cases := []struct {
		name string
		cfg  *Config
		want error
	}{
		{"nil", nil, ErrEmptyConfig},
		{"empty", &Config{Version: Version2, Flags: map[string]*Flag{}}, ErrEmptyConfig},
		{"self-loop", installableConfig(handFlag("a", "a")), ErrCircularDependency},
		{"cycle", installableConfig(handFlag("a", "b"), handFlag("b", "a")), ErrCircularDependency},
		{"missing", installableConfig(handFlag("a", "ghost")), ErrMissingDependency},
		{"contradiction", installableConfig(handFlag("a", "off"),
			&Flag{Key: "off", On: false}), ErrDependencyContradiction},
		{"dup-prereq", installableConfig(handFlag("a", "b", "b"), handFlag("b")),
			ErrDependencyContradiction},
		{"bad-weights", &Config{Version: Version2, Flags: map[string]*Flag{"a": {
			Key: "a", On: true,
			Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
				Variants: []Variant{{Key: "x", WeightBPS: 5000}}}},
		}}}, ErrWeightSum},
		{"bad-percentage", &Config{Version: Version2, Flags: map[string]*Flag{"a": {
			Key: "a", On: true,
			Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 140,
				Variants: []Variant{{Key: "x", WeightBPS: 10000}}}},
		}}}, ErrBadPercentage},
		{"key-mismatch", &Config{Version: Version2, Flags: map[string]*Flag{"a": {
			Key: "b", On: true,
			Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
				Variants: []Variant{{Key: "x", WeightBPS: 10000}}}},
		}}}, ErrFlagKeyMismatch},
		{"unknown-operator", &Config{Version: Version2, Flags: map[string]*Flag{"a": {
			Key: "a", On: true,
			Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
				Conditions: []Condition{{Attribute: "tier", Operator: "regex", Values: []string{"gold"}}},
				Variants:   []Variant{{Key: "x", WeightBPS: 10000}}}},
		}}}, ErrUnknownOperator},
		{"in-without-values", &Config{Version: Version2, Flags: map[string]*Flag{"a": {
			Key: "a", On: true,
			Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
				Conditions: []Condition{{Attribute: "tier", Operator: OpIn}},
				Variants:   []Variant{{Key: "x", WeightBPS: 10000}}}},
		}}}, ErrBadCondition},
		{"not-in-without-values", &Config{Version: Version2, Flags: map[string]*Flag{"a": {
			Key: "a", On: true,
			Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
				Conditions: []Condition{{Attribute: "tier", Operator: OpNotIn}},
				Variants:   []Variant{{Key: "x", WeightBPS: 10000}}}},
		}}}, ErrBadCondition},
		{"empty-condition-attribute", &Config{Version: Version2, Flags: map[string]*Flag{"a": {
			Key: "a", On: true,
			Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
				Conditions: []Condition{{Operator: OpEqual, Values: []string{"x"}}},
				Variants:   []Variant{{Key: "x", WeightBPS: 10000}}}},
		}}}, ErrBadCondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := e.Install(tc.cfg)
			if err == nil {
				t.Fatal("expected rejection, got nil")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			// Old config still serving, never an empty result.
			r := e.Evaluate(context.Background(), User{ID: "u"}, "base")
			if !r.On || r.Variant != "on" || r.Status == "" {
				t.Fatalf("previous config not serving after rejection: %+v", r)
			}
		})
	}
}

// The hand-built (Install) and JSON (ParseConfig) entries must agree on what
// is illegal: for each malformed shape both entries reject with the same
// sentinel error, and neither leaves a broken config behind.
func TestInstall_ParseParity(t *testing.T) {
	cases := []struct {
		name string
		json string
		hand *Config
		want error
	}{
		{
			name: "unknown-operator",
			json: `{"version":"v2","flags":[{"key":"a","on":true,"rules":[
				{"id":"r","priority":1,"conditions":[{"attribute":"tier","operator":"regex","values":["gold"]}],
				 "variants":[{"key":"x","weightBPS":10000}]}]}]}`,
			hand: &Config{Version: Version2, Flags: map[string]*Flag{"a": {
				Key: "a", On: true,
				Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
					Conditions: []Condition{{Attribute: "tier", Operator: "regex", Values: []string{"gold"}}},
					Variants:   []Variant{{Key: "x", WeightBPS: 10000}}}},
			}}},
			want: ErrUnknownOperator,
		},
		{
			name: "in-without-values",
			json: `{"version":"v2","flags":[{"key":"a","on":true,"rules":[
				{"id":"r","priority":1,"conditions":[{"attribute":"tier","operator":"in"}],
				 "variants":[{"key":"x","weightBPS":10000}]}]}]}`,
			hand: &Config{Version: Version2, Flags: map[string]*Flag{"a": {
				Key: "a", On: true,
				Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
					Conditions: []Condition{{Attribute: "tier", Operator: OpIn}},
					Variants:   []Variant{{Key: "x", WeightBPS: 10000}}}},
			}}},
			want: ErrBadCondition,
		},
		{
			name: "not-in-without-values",
			json: `{"version":"v2","flags":[{"key":"a","on":true,"rules":[
				{"id":"r","priority":1,"conditions":[{"attribute":"tier","operator":"not_in"}],
				 "variants":[{"key":"x","weightBPS":10000}]}]}]}`,
			hand: &Config{Version: Version2, Flags: map[string]*Flag{"a": {
				Key: "a", On: true,
				Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
					Conditions: []Condition{{Attribute: "tier", Operator: OpNotIn}},
					Variants:   []Variant{{Key: "x", WeightBPS: 10000}}}},
			}}},
			want: ErrBadCondition,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := ParseConfig([]byte(tc.json))
			if !errors.Is(perr, tc.want) {
				t.Fatalf("ParseConfig: want %v, got %v", tc.want, perr)
			}
			e := NewEngine()
			if err := e.Install(installableConfig(handFlag("base"))); err != nil {
				t.Fatalf("seed install: %v", err)
			}
			ierr := e.Install(tc.hand)
			if !errors.Is(ierr, tc.want) {
				t.Fatalf("Install: want %v, got %v", tc.want, ierr)
			}
			// A rejected hand-built config must not have replaced the seed.
			if r := e.Evaluate(context.Background(), User{ID: "u"}, "base"); !r.On {
				t.Fatalf("seed config not serving after rejected install: %+v", r)
			}
		})
	}
}

// A valid hand-built config must install and serve immediately.
func TestInstall_ValidSwap(t *testing.T) {
	e := NewEngine()
	if err := e.Install(installableConfig(handFlag("a"))); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	next := installableConfig(handFlag("a"), handFlag("b", "a"))
	if err := e.Install(next); err != nil {
		t.Fatalf("install v2: %v", err)
	}
	r := e.Evaluate(context.Background(), User{ID: "u"}, "b")
	if !r.On {
		t.Fatalf("new config not serving: %+v", r)
	}
}

// Concurrent Install (valid and invalid) and Evaluate must never crash,
// block, or yield an empty/invalid result.
func TestInstall_ConcurrentWithEvaluate(t *testing.T) {
	e := NewEngine()
	if err := e.Install(installableConfig(handFlag("base"))); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	valid := installableConfig(handFlag("base"), handFlag("extra", "base"))
	invalid := installableConfig(handFlag("base", "base")) // self-loop
	// Invalid via the structural checks added to Install (conditions and
	// key identity), so concurrent swaps exercise both rejection paths.
	invalidCond := installableConfig(&Flag{Key: "base", On: true, Rules: []*Rule{{
		ID: "r", Priority: 1, Percentage: 100,
		Conditions: []Condition{{Attribute: "tier", Operator: "weird"}},
		Variants:   []Variant{{Key: "on", WeightBPS: 10000}},
	}}})
	invalidMismatch := &Config{Version: Version2, Flags: map[string]*Flag{"base": {
		Key: "other", On: true,
		Rules: []*Rule{{ID: "r", Priority: 1, Percentage: 100,
			Variants: []Variant{{Key: "on", WeightBPS: 10000}}}},
	}}}

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
				if r.Status == "" || r.MatchedRule == "" || !r.On {
					select {
					case errs <- "bad result: " + r.Reason:
					default:
					}
					return
				}
			}
		}(w)
	}

	// Flip configs rapidly, mixing valid swaps with rejections.
	for i := 0; i < 200; i++ {
		if err := e.Install(valid); err != nil {
			t.Fatalf("valid install rejected: %v", err)
		}
		bad := invalid
		switch i % 3 {
		case 1:
			bad = invalidCond
		case 2:
			bad = invalidMismatch
		}
		if err := e.Install(bad); err == nil {
			t.Fatal("invalid config installed")
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}
	// Final state must be the last valid config (invalid never installed).
	if r := e.Evaluate(context.Background(), User{ID: "u"}, "extra"); !r.On {
		t.Fatalf("final valid config not serving: %+v", r)
	}
}
