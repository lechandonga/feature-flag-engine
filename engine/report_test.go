package engine

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memorySink 记录发送内容；可配置失败。
type memorySink struct {
	mu    sync.Mutex
	got   []Attribution
	failN int32 // 前 N 次发送失败
	calls int32
}

func (m *memorySink) Send(a Attribution) error {
	n := atomic.AddInt32(&m.calls, 1)
	if n <= atomic.LoadInt32(&m.failN) {
		return errors.New("send failed")
	}
	m.mu.Lock()
	m.got = append(m.got, a)
	m.mu.Unlock()
	return nil
}

func (m *memorySink) snapshot() []Attribution {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Attribution(nil), m.got...)
}

func TestReporterDedupWithinWindow(t *testing.T) {
	sink := &memorySink{}
	r := NewChannelReporter(sink, ReporterOptions{
		QueueSize: 16,
		DedupeTTL: time.Minute,
	})
	defer r.Close()

	a := Attribution{FlagKey: "f", UserID: "u1", Variation: "on", Source: SourceRule, HitRuleID: "r1", ConfigVersion: 2}
	for i := 0; i < 10; i++ {
		r.Record(a)
	}
	r.Close()

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 report, got %d", len(got))
	}
	if r.SentCount() != 1 {
		t.Fatalf("sent count = %d", r.SentCount())
	}
	if r.DroppedCount() != 9 {
		t.Fatalf("dropped count = %d, want 9", r.DroppedCount())
	}
}

func TestReporterDifferentResultReported(t *testing.T) {
	sink := &memorySink{}
	r := NewChannelReporter(sink, ReporterOptions{QueueSize: 16, DedupeTTL: time.Minute})
	r.Record(Attribution{FlagKey: "f", UserID: "u1", Variation: "on", Source: SourceRule,
		DedupeKey: "f|u1|on|rule|"})
	r.Record(Attribution{FlagKey: "f", UserID: "u1", Variation: "off", Source: SourceDefault,
		DedupeKey: "f|u1|off|default|"})
	r.Close()
	if len(sink.snapshot()) != 2 {
		t.Fatalf("expected 2 reports for different results, got %d", len(sink.snapshot()))
	}
}

func TestReporterQueueFullNeverBlocks(t *testing.T) {
	// 阻塞型 sink：先不消费，让队列打满；Record 必须立刻返回。
	block := make(chan struct{})
	sink := ReportSinkFunc(func(a Attribution) error {
		<-block
		return nil
	})
	r := NewChannelReporter(sink, ReporterOptions{QueueSize: 4, DedupeTTL: time.Minute})

	done := make(chan struct{})
	go func() {
		// 每条去重 key 不同，强制走队列。
		for i := 0; i < 1000; i++ {
			r.Record(Attribution{FlagKey: "f", UserID: "u" + itoa(i), DedupeKey: itoa(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked when queue was full")
	}
	close(block)
	_ = r.Close()
}

func TestReporterFailureDoesNotAffectEvaluation(t *testing.T) {
	sink := &memorySink{failN: 1 << 30} // 永远发送失败
	e := New(WithReporter(NewChannelReporter(sink, ReporterOptions{QueueSize: 8, DedupeTTL: time.Millisecond})))
	if err := e.LoadConfig(mustParse(t, goodCfgV2On)); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 200; i++ {
		r := e.Evaluate("f", User{ID: "u" + itoa(i)})
		if r.Status != StatusOK || r.Variation != "on" {
			t.Fatalf("evaluation affected by reporter failure: %+v", r)
		}
	}
	// 显式关闭以排空队列（关闭发送也会尝试失败 sink），再断言。
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if sink.calls == 0 {
		t.Fatal("expected sink to be attempted")
	}
	if len(sink.snapshot()) != 0 {
		t.Fatal("nothing should be successfully sent")
	}
}

// ReportSinkFunc 让函数直接实现 ReportSink。
type ReportSinkFunc func(a Attribution) error

func (f ReportSinkFunc) Send(a Attribution) error { return f(a) }
