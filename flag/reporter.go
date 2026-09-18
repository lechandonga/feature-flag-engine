package flag

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Attribution is one evaluation event reported for analytics.
type Attribution struct {
	FlagKey       string
	UserID        string
	Status        Status
	Variant       string
	Source        Source
	MatchedRule   string
	ConfigVersion Version
	ObservedAt    time.Time
}

// Sink consumes attribution events. Implementations must be safe for
// concurrent use.
type Sink interface {
	Report(ctx context.Context, a Attribution) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, a Attribution) error

func (f SinkFunc) Report(ctx context.Context, a Attribution) error { return f(ctx, a) }

// defaultDedupeWindow bounds how long "same outcome" is suppressed for one
// (flag,user) pair. After the window elapses the outcome is reported again
// even if unchanged.
const defaultDedupeWindow = 5 * time.Minute

// defaultQueueSize bounds in-memory pending events. When the queue is full
// events are dropped (counted) rather than blocking the evaluation path.
const defaultQueueSize = 1024

type seenEntry struct {
	signature string
	at        time.Time
}

// Reporter dedupes repeated (flagKey,userID) outcomes and forwards them
// asynchronously. Reporting is strictly off the evaluation critical path:
//
//   - Record never blocks: it hands the event to a buffered queue and
//     returns immediately;
//   - sink errors (or a slow/hung sink) are counted and retried at most
//     once, never propagated to the caller of Record;
//   - identical outcomes for the same (flag,user) within the dedupe window
//     are suppressed; the first occurrence and every change are forwarded.
type Reporter struct {
	sink      Sink
	dedupeFor time.Duration
	queue     chan Attribution
	dropCount atomic.Int64
	sentCount atomic.Int64
	failCount atomic.Int64

	mu   sync.Mutex
	seen map[string]seenEntry

	stop chan struct{}
	wg   sync.WaitGroup
}

// ReporterOption customises a Reporter.
type ReporterOption func(*Reporter)

// WithDedupeWindow sets how long an unchanged outcome stays suppressed.
func WithDedupeWindow(d time.Duration) ReporterOption {
	return func(r *Reporter) {
		if d > 0 {
			r.dedupeFor = d
		}
	}
}

// WithQueueSize sets the asynchronous event buffer capacity.
func WithQueueSize(n int) ReporterOption {
	return func(r *Reporter) {
		if n > 0 {
			r.queue = make(chan Attribution, n)
		}
	}
}

// NewReporter creates and starts a reporter around a sink.
func NewReporter(sink Sink, opts ...ReporterOption) *Reporter {
	r := &Reporter{
		sink:      sink,
		dedupeFor: defaultDedupeWindow,
		queue:     make(chan Attribution, defaultQueueSize),
		seen:      map[string]seenEntry{},
		stop:      make(chan struct{}),
	}
	for _, o := range opts {
		o(r)
	}
	r.wg.Add(1)
	go r.loop()
	return r
}

// FromResult builds an Attribution from an evaluation Result.
func FromResult(res Result) Attribution {
	return Attribution{
		FlagKey:       res.FlagKey,
		UserID:        res.UserID,
		Status:        res.Status,
		Variant:       res.Variant,
		Source:        res.Source,
		MatchedRule:   res.MatchedRule,
		ConfigVersion: res.ConfigVersion,
		ObservedAt:    time.Now(),
	}
}

// Record enqueues an attribution if the outcome changed, was never seen, or
// the dedupe window elapsed. It returns immediately and never returns an
// error: reporting cannot affect evaluation.
func (r *Reporter) Record(ctx context.Context, a Attribution) {
	if a.ObservedAt.IsZero() {
		a.ObservedAt = time.Now()
	}
	if !r.shouldReport(a) {
		return
	}
	select {
	case r.queue <- a:
	default:
		// Queue saturated: drop rather than block the evaluation path.
		r.dropCount.Add(1)
	}
}

func (r *Reporter) shouldReport(a Attribution) bool {
	key := a.FlagKey + "\x00" + a.UserID
	sig := string(a.Status) + "|" + a.Variant + "|" + string(a.Source) + "|" + a.MatchedRule + "|" + string(a.ConfigVersion)
	now := a.ObservedAt

	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.seen[key]
	if ok && prev.signature == sig && now.Sub(prev.at) < r.dedupeFor {
		return false
	}
	r.seen[key] = seenEntry{signature: sig, at: now}
	return true
}

func (r *Reporter) loop() {
	defer r.wg.Done()
	for {
		// Prioritise shutdown: when stop is closed and the queue still has
		// events we must abandon the queue promptly rather than drain it.
		select {
		case <-r.stop:
			return
		default:
		}
		select {
		case <-r.stop:
			return
		case a := <-r.queue:
			r.deliver(a)
		}
	}
}

// deliver sends one event with a bounded timeout and a single immediate
// retry on failure. Sink problems never escape the reporter.
func (r *Reporter) deliver(a Attribution) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.sink.Report(ctx, a); err != nil {
		if err2 := r.sink.Report(ctx, a); err2 != nil {
			r.failCount.Add(1)
			return
		}
	}
	r.sentCount.Add(1)
}

// Stats reports counters: delivered, permanently failed, and dropped due to
// a full queue.
func (r *Reporter) Stats() (sent, failed, dropped int64) {
	return r.sentCount.Load(), r.failCount.Load(), r.dropCount.Load()
}

// Close stops the worker. In-flight events already handed to the sink run to
// completion (bounded by the sink timeout); queued-but-undelivered events at
// shutdown are intentionally not flushed to avoid blocking process exit.
func (r *Reporter) Close() error {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	r.wg.Wait()
	return nil
}
