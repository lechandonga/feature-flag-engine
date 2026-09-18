package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lechandonga/feature-flag-engine/flag"
)

func main() {
	ctx := context.Background()

	data, err := os.ReadFile("configs/example.v2.json")
	must("read config", err)

	cfg, err := flag.ParseConfig(data)
	must("parse config", err)
	fmt.Printf("loaded config %s with %d flags\n", cfg.Version, len(cfg.Flags))

	// Sink just prints attributions; failures here can never affect Evaluate.
	sink := flag.SinkFunc(func(ctx context.Context, a flag.Attribution) error {
		fmt.Printf("    [attribution] user=%s flag=%s status=%s variant=%q rule=%s\n",
			a.UserID, a.FlagKey, a.Status, a.Variant, a.MatchedRule)
		return nil
	})
	reporter := flag.NewReporter(sink, flag.WithDedupeWindow(time.Minute))
	defer reporter.Close()

	engine := flag.NewEngine()
	engine.Snapshot(cfg)

	evaluate := func(label string, user flag.User, key string) {
		res := engine.Evaluate(ctx, user, key)
		fmt.Printf("%-28s => on=%-5v variant=%-9s source=%-9s reason=%s\n",
			label, res.On, res.Variant, res.Source, res.Reason)
		reporter.Record(ctx, flag.FromResult(res))
	}

	users := []struct {
		label string
		user  flag.User
	}{
		{"employee", flag.User{ID: "alice", Attributes: map[string]string{"tier": "employee", "country": "US"}}},
		{"beta US user", flag.User{ID: "bob", Attributes: map[string]string{"tier": "free", "country": "US"}}},
		{"EU user", flag.User{ID: "carol", Attributes: map[string]string{"tier": "free", "country": "DE"}}},
	}
	for _, u := range users {
		evaluate(u.label, u.user, "checkout-redesign")
		evaluate(u.label+" / one-click", u.user, "one-click-pay")
	}

	// Repeated evaluation of the same pair: deduped in the reporter.
	evaluate("repeat bob", flag.User{ID: "bob", Attributes: map[string]string{"tier": "free", "country": "US"}}, "checkout-redesign")

	time.Sleep(50 * time.Millisecond) // let async attribution flush
	sent, failed, dropped := reporter.Stats()
	fmt.Printf("attribution stats: sent=%d failed=%d dropped=%d\n", sent, failed, dropped)
}

func must(label string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", label, err)
		os.Exit(1)
	}
}
