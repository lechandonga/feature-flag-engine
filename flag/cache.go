package flag

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Loader produces a raw configuration payload (file read, HTTP pull, ...).
type Loader interface {
	Load(ctx context.Context) ([]byte, error)
}

// LoaderFunc adapts a function to Loader.
type LoaderFunc func(ctx context.Context) ([]byte, error)

func (f LoaderFunc) Load(ctx context.Context) ([]byte, error) { return f(ctx) }

// RefreshStats summarises the latest refresh outcome.
type RefreshStats struct {
	LastOK      bool
	LastError   string
	LastVersion Version
	UpdatedAt   time.Time
	Successes   int64
	Failures    int64
}

// Refresher polls a Loader in the background and installs new configs
// atomically. Guarantees:
//
//   - a failed load, an unparseable payload, or a rejected (e.g. cyclic)
//     config never touches the engine: evaluations continue against the
//     last successfully installed snapshot;
//   - the new Config is fully built and validated before Snapshot swaps it
//     in, so evaluations never observe a half-built configuration;
//   - refreshes are serialised (no overlapping load/parse/install);
//   - before the first successful refresh, evaluations return an
//     explainable "unavailable" result instead of an empty value.
type Refresher struct {
	engine *Engine
	loader Loader

	refreshMu sync.Mutex // serialises refresh cycles

	mu      sync.RWMutex
	stats   RefreshStats
	lastRaw []byte // last successfully installed raw payload (dedupe)

	stop chan struct{}
	done chan struct{}
}

// NewRefresher creates a refresher for the given engine.
func NewRefresher(e *Engine, loader Loader) *Refresher {
	return &Refresher{engine: e, loader: loader, stop: make(chan struct{})}
}

// Start launches the background polling loop. The first refresh happens
// immediately so callers do not wait a full interval for initial config.
func (r *Refresher) Start(ctx context.Context, interval time.Duration) {
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		_ = r.RefreshOnce(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = r.RefreshOnce(ctx)
			}
		}
	}()
}

// RefreshOnce performs one refresh cycle: load -> parse/validate (including
// dependency checks) -> atomic install. Any error is recorded and returned;
// the previously active configuration is left completely untouched.
//
// An unchanged payload (byte-identical to the last installed one) is skipped
// without touching the engine.
func (r *Refresher) RefreshOnce(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	data, err := r.loader.Load(ctx)
	if err != nil {
		r.recordFailure(err)
		return err
	}
	if r.lastRaw != nil && bytesEqual(r.lastRaw, data) {
		// Payload unchanged: nothing to install; not counted as a failure.
		return nil
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		r.recordFailure(err)
		return err
	}

	// Install only after the whole config is validated. Store is atomic;
	// in-flight evaluations finish against the old snapshot, new ones see
	// the new one. No intermediate state is ever observable.
	r.engine.Snapshot(cfg)
	r.lastRaw = append([]byte(nil), data...)

	r.mu.Lock()
	r.stats.LastOK = true
	r.stats.LastError = ""
	r.stats.LastVersion = cfg.Version
	r.stats.UpdatedAt = time.Now()
	r.stats.Successes++
	r.mu.Unlock()
	return nil
}

func (r *Refresher) recordFailure(err error) {
	atomic.AddInt64(&r.stats.Failures, 1) // keep Failures count cheap to read
	r.mu.Lock()
	r.stats.LastOK = false
	r.stats.LastError = err.Error()
	r.stats.Failures = atomic.LoadInt64(&r.stats.Failures)
	r.mu.Unlock()
}

// Stats returns a copy of the latest refresh outcome.
func (r *Refresher) Stats() RefreshStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.stats
	s.Failures = atomic.LoadInt64(&r.stats.Failures)
	return s
}

// Close stops the background loop and waits for it to exit.
func (r *Refresher) Close() {
	select {
	case <-r.stop:
		// already closed
	default:
		close(r.stop)
	}
	if r.done != nil {
		<-r.done
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
