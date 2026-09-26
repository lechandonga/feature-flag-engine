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
		if err := e.Install(invalid); err == nil {
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
