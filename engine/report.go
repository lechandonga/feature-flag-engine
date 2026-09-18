package engine

import (
	"sync"
	"sync/atomic"
	"time"
)

// 默认上报参数。
const (
	defaultQueueSize = 256
	defaultDedupeTTL = 5 * time.Minute
)

// Reporter 是评估归因上报器抽象。上报必须与评估解耦：
// 上报失败、阻塞或关闭都不得影响评估链路。
type Reporter interface {
	Record(a Attribution)
	Close() error
}

// ReportSink 表示真正的上报出口（HTTP、日志、消息队列等由调用方实现）。
type ReportSink interface {
	Send(a Attribution) error
}

// ReporterOptions 配置 ChannelReporter 的背压与去重行为。
type ReporterOptions struct {
	// QueueSize 为异步队列容量；队列满时新归因被丢弃（仅计数），绝不阻塞评估。
	QueueSize int
	// DedupeTTL 为去重窗口；窗口内相同 (flag,user,variation,rule) 只上报一次。
	DedupeTTL time.Duration
}

// ChannelReporter 是 Reporter 的默认异步实现：非阻塞入队 + 单 worker 顺序发送
// + 时间窗口去重。
type ChannelReporter struct {
	opts    ReporterOptions
	sink    ReportSink
	ch      chan Attribution
	stop    chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Int64
	sent    atomic.Int64

	dedupeMu sync.Mutex
	seen     map[string]time.Time
}

// NewChannelReporter 创建异步上报器并启动后台 worker。
func NewChannelReporter(sink ReportSink, opts ReporterOptions) *ChannelReporter {
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if opts.DedupeTTL <= 0 {
		opts.DedupeTTL = defaultDedupeTTL
	}
	r := &ChannelReporter{
		opts: opts,
		sink: sink,
		ch:   make(chan Attribution, opts.QueueSize),
		stop: make(chan struct{}),
		seen: make(map[string]time.Time),
	}
	r.wg.Add(1)
	go r.run()
	return r
}

// run 是单 worker：顺序消费队列，发送失败只计数，不重试、不影响评估。
func (r *ChannelReporter) run() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.opts.DedupeTTL)
	defer ticker.Stop()
	for {
		select {
		case a := <-r.ch:
			if err := r.sink.Send(a); err != nil {
				// 上报失败：仅计入 dropped，不回压评估链路。
				r.dropped.Add(1)
				continue
			}
			r.sent.Add(1)
		case <-ticker.C:
			r.gcSeen(time.Now())
		case <-r.stop:
			// 关闭时尽力排空当前队列中已入队的归因（非阻塞），随后退出。
			for {
				select {
				case a := <-r.ch:
					if err := r.sink.Send(a); err != nil {
						r.dropped.Add(1)
					} else {
						r.sent.Add(1)
					}
				default:
					return
				}
			}
		}
	}
}

// Record 非阻塞记录一次归因；重复、队列满或已关闭都不会影响调用方。
func (r *ChannelReporter) Record(a Attribution) {
	if a.DedupeKey == "" {
		// 理论上 Engine 总是会填 DedupeKey，这里做兜底。
		a.DedupeKey = a.FlagKey + "|" + a.UserID + "|" + a.Variation + "|" + a.HitRuleID
	}
	if !r.markSeen(a.DedupeKey, time.Now()) {
		r.dropped.Add(1)
		return
	}
	select {
	case r.ch <- a:
	default:
		// 队列满：丢弃本次上报，绝不阻塞评估。
		r.dropped.Add(1)
	}
}

// markSeen 在去重窗口内保证同一 key 只通过一次；惰性清理过期项。
func (r *ChannelReporter) markSeen(key string, now time.Time) bool {
	r.dedupeMu.Lock()
	defer r.dedupeMu.Unlock()
	if t, ok := r.seen[key]; ok && now.Sub(t) < r.opts.DedupeTTL {
		return false
	}
	r.seen[key] = now
	// 顺带做一次轻量惰性回收，避免 map 无限增长。
	if len(r.seen) > 1024 {
		for k, t := range r.seen {
			if now.Sub(t) >= r.opts.DedupeTTL {
				delete(r.seen, k)
			}
		}
	}
	return true
}

func (r *ChannelReporter) gcSeen(now time.Time) {
	r.dedupeMu.Lock()
	defer r.dedupeMu.Unlock()
	for k, t := range r.seen {
		if now.Sub(t) >= r.opts.DedupeTTL {
			delete(r.seen, k)
		}
	}
}

// Close 停止 worker 并尽力发送队列内剩余归因。
func (r *ChannelReporter) Close() error {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	r.wg.Wait()
	return nil
}

// DroppedCount 返回因去重、队列满或发送失败而未成功上报的归因数量。
func (r *ChannelReporter) DroppedCount() int64 { return r.dropped.Load() }

// SentCount 返回成功发送的归因数量。
func (r *ChannelReporter) SentCount() int64 { return r.sent.Load() }

// noopReporter 在调用方不关心归因时使用。
type noopReporter struct{}

func (noopReporter) Record(Attribution) {}
func (noopReporter) Close() error       { return nil }

// NewNoopReporter 返回一个丢弃所有归因的上报器。
func NewNoopReporter() Reporter { return noopReporter{} }
