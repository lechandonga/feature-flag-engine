package flag

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingSink struct {
	mu     sync.Mutex
	events []Attribution
}

func (s *recordingSink) Report(ctx context.Context, a Attribution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, a)
	return nil
}

func (s *recordingSink) snapshot() []Attribution {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Attribution, len(s.events))
	copy(out, s.events)
	return out
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting: " + msg)
}

func TestReporter_DedupesRepeatedSameOutcome(t *testing.T) {
	sink := &recordingSink{}
	r := NewReporter(sink)
	defer r.Close()

	ctx := context.Background()
	a := Attribution{FlagKey: "f", UserID: "u1", Status: StatusOn, Variant: "v1",
		Source: SourceRule, MatchedRule: "rule-1", ConfigVersion: Version2}
	for i := 0; i < 20; i++ {
		r.Record(ctx, a)
	}
	waitFor(t, func() bool { return len(sink.snapshot()) >= 1 }, "first event")
	time.Sleep(50 * time.Millisecond)
	if got := len(sink.snapshot()); got != 1 {
		t.Fatalf("expected exactly 1 report, got %d", got)
	}

	// Outcome change for same user/flag is reported.
	b := a
	b.Variant = "v2"
	r.Record(ctx, b)
	waitFor(t, func() bool { return len(sink.snapshot()) >= 2 }, "changed event")
}

func TestReporter_DifferentUsersReported(t *testing.T) {
	sink := &recordingSink{}
	r := NewReporter(sink)
	defer r.Close()

	for _, u := range []string{"u1", "u2", "u3"} {
		r.Record(context.Background(), Attribution{FlagKey: "f", UserID: u,
			Status: StatusOn, Variant: "v", Source: SourceRule})
	}
	waitFor(t, func() bool { return len(sink.snapshot()) == 3 }, "three users")
}

func TestReporter_DedupeWindowElapses(t *testing.T) {
	sink := &recordingSink{}
	r := NewReporter(sink, WithDedupeWindow(20*time.Millisecond))
	defer r.Close()

	a := Attribution{FlagKey: "f", UserID: "u", Status: StatusOn, Variant: "v", Source: SourceRule}
	r.Record(context.Background(), a)
	time.Sleep(40 * time.Millisecond)
	r.Record(context.Background(), a)
	waitFor(t, func() bool { return len(sink.snapshot()) >= 2 }, "re-report after window")
}

type failingSink struct {
	fails atomic.Int32
}

func (s *failingSink) Report(ctx context.Context, a Attribution) error {
	s.fails.Add(1)
	return errors.New("sink unavailable")
}

func TestReporter_SinkFailure_DoesNotBlockOrError(t *testing.T) {
	sink := &failingSink{}
	r := NewReporter(sink)
	defer r.Close()

	ctx := context.Background()
	a := Attribution{FlagKey: "f", UserID: "u", Status: StatusOn, Variant: "v", Source: SourceRule}
	// Distinct users to bypass dedupe; each Record must return instantly.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			a2 := a
			a2.UserID = "u" + itoaTest(i)
			r.Record(ctx, a2)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked despite failing sink")
	}
	waitFor(t, func() bool { _, failed, _ := r.Stats(); return failed > 0 }, "failure counters")
	// Evaluation-facing API has no error: nothing to check beyond no panic.
}

type slowSink struct {
	inside atomic.Int32
	delay  time.Duration
}

func (s *slowSink) Report(ctx context.Context, a Attribution) error {
	s.inside.Add(1)
	d := s.delay
	if d == 0 {
		d = 500 * time.Millisecond
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
		s.inside.Add(-1)
		return ctx.Err()
	}
	s.inside.Add(-1)
	return nil
}

func TestReporter_SlowSink_DoesNotBlockRecord(t *testing.T) {
	sink := &slowSink{}
	r := NewReporter(sink, WithQueueSize(8))
	defer r.Close()

	a := Attribution{FlagKey: "f", Status: StatusOn, Source: SourceRule}
	start := time.Now()
	for i := 0; i < 64; i++ {
		a2 := a
		a2.UserID = "u" + itoaTest(i)
		r.Record(context.Background(), a2)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("enqueue took %v, must not wait on slow sink", elapsed)
	}
}

func TestReporter_FullQueue_DropsAndCounts(t *testing.T) {
	// Block the worker from draining by keeping Close uncalled and sink slow;
	// small queue overflows quickly; dropped events must be counted.
	sink := &slowSink{delay: 20 * time.Millisecond}
	r := NewReporter(sink, WithQueueSize(4))
	defer r.Close()

	a := Attribution{FlagKey: "f", Status: StatusOn, Source: SourceRule}
	for i := 0; i < 100; i++ {
		a2 := a
		a2.UserID = "u" + itoaTest(i)
		r.Record(context.Background(), a2)
	}
	_, _, dropped := r.Stats()
	if dropped == 0 {
		t.Fatal("expected some drops with a saturated queue")
	}
}

func TestReporter_IntegratesWithEngineResult(t *testing.T) {
	sink := &recordingSink{}
	rep := NewReporter(sink)
	defer rep.Close()

	cfg, err := ParseConfig(buildConfig(t, onFlag("f")))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine()
	e.Snapshot(cfg)
	res := e.Evaluate(context.Background(), User{ID: "u1"}, "f")
	rep.Record(context.Background(), FromResult(res))

	waitFor(t, func() bool { return len(sink.snapshot()) == 1 }, "integrated event")
	ev := sink.snapshot()[0]
	if ev.FlagKey != "f" || ev.UserID != "u1" || !strings.Contains(ev.MatchedRule, "f-r") {
		t.Fatalf("attribution content wrong: %+v", ev)
	}
}
