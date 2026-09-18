package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Engine 是特性开关评估引擎。
// 内部持有一份不可变配置快照（atomic.Pointer），刷新时整体替换：
// 评估永远基于某个一致快照，刷新中间状态不会被观察到。
type Engine struct {
	current  atomic.Pointer[snapshot]
	reporter Reporter

	source ConfigSource

	lastErr atomic.Pointer[ConfigError]

	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// snapshot 是一次成功加载产生的不可变快照。评估全程只读它。
type snapshot struct {
	eval     *evaluator
	version  int
	source   string
	stale    bool
	loadedAt time.Time
}

// Option 配置 Engine。
type Option func(*Engine)

// WithReporter 设置归因上报器；不设置时使用 noopReporter。
func WithReporter(r Reporter) Option {
	return func(e *Engine) {
		if r != nil {
			e.reporter = r
		}
	}
}

// New 创建引擎。此时尚无配置，Evaluate 会返回 StatusUnavailable，
// 直到 Load / LoadConfig / StartRefresher 成功加载过一次。
func New(opts ...Option) *Engine {
	e := &Engine{
		reporter: NewNoopReporter(),
		stop:     make(chan struct{}),
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Load 从给定数据源同步加载并校验一次配置，并记录为刷新数据源。
// 配置被拒绝（解析失败/校验失败）或读取失败时：保留旧快照不变，
// 已有评估行为不受影响，并返回可区分原因。
func (e *Engine) Load(ctx context.Context, src ConfigSource) error {
	e.source = src
	return e.refreshOnce(ctx)
}

// LoadConfig 直接接受一份已归一化的配置（主要用于测试与本地直配）。
// 仍会执行 ValidateConfig，保证所有入口的校验语义一致；校验失败则拒绝换入。
func (e *Engine) LoadConfig(cfg *Config) error {
	if err := ValidateConfig(cfg); err != nil {
		e.lastErr.Store(asConfigError(err))
		return err
	}
	e.swap(cfg, false)
	e.lastErr.Store(nil)
	return nil
}

// StartRefresher 先同步加载一次，再启动后台定时刷新。
// 初次加载失败不会启动循环（返回错误，引擎仍处于 unavailable）；
// 之后的刷新失败只标记 stale，继续使用最近一次有效配置。
func (e *Engine) StartRefresher(ctx context.Context, src ConfigSource, interval time.Duration) error {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if err := e.Load(ctx, src); err != nil {
		return err
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = e.refreshOnce(ctx)
			case <-e.stop:
				return
			}
		}
	}()
	return nil
}

// refreshOnce 执行一次加载->解析->校验->原子换入。
// 任何一步失败都不会触碰 current（除非此前已有快照，则把它标记为 stale）。
func (e *Engine) refreshOnce(ctx context.Context) error {
	src := e.source
	if src == nil {
		err := cfgError(ReasonConfigUnavailable, "", "no config source configured")
		e.lastErr.Store(err)
		return err
	}
	data, source, err := src.Load(ctx)
	if err != nil {
		return e.failRefresh(cfgError(ReasonConfigUnavailable, "source", "load failed: "+err.Error()))
	}
	cfg, perr := ParseConfig(data, source)
	if perr != nil {
		return e.failRefresh(asConfigError(perr))
	}
	e.swap(cfg, false)
	e.lastErr.Store(nil)
	return nil
}

// failRefresh 在刷新失败时保留旧快照；若存在旧快照则将其标记为 stale。
func (e *Engine) failRefresh(err *ConfigError) error {
	e.lastErr.Store(err)
	if old := e.current.Load(); old != nil {
		e.current.Store(&snapshot{
			eval:     old.eval,
			version:  old.version,
			source:   old.source,
			stale:    true,
			loadedAt: old.loadedAt,
		})
	}
	return err
}

// swap 原子换入新快照。
func (e *Engine) swap(cfg *Config, stale bool) {
	e.current.Store(&snapshot{
		eval:     newEvaluator(cfg),
		version:  cfg.Version,
		source:   cfg.Source,
		stale:    stale,
		loadedAt: time.Now(),
	})
}

// Evaluate 评估指定用户在指定开关上的分组。
// 永不因上报或刷新内部状态而阻塞 / panic：上报走异步队列。
func (e *Engine) Evaluate(flagKey string, user User) Result {
	snap := e.current.Load()
	if snap == nil {
		// 没有任何有效快照：明确告诉调用方“配置不可用”，而不是返回空值/默认值
		// 造成误判。调用方可据此使用自己的兜底策略。
		return Result{
			Status:  StatusUnavailable,
			FlagKey: flagKey,
			Reason:  ReasonConfigUnavailable,
		}
	}
	res := snap.eval.evaluate(flagKey, user, nil)
	res.Stale = snap.stale

	// 归因仅针对真实存在的开关的有效评估；unavailable/not_found 不上报。
	if res.Status == StatusOK || res.Status == StatusPrerequisiteNotMet {
		e.reporter.Record(Attribution{
			FlagKey:       res.FlagKey,
			UserID:        user.ID,
			Variation:     res.Variation,
			Source:        res.Source,
			HitRuleID:     res.HitRuleID,
			ConfigVersion: res.ConfigVersion,
			DedupeKey:     dedupeKey(res, user.ID),
		})
	}
	return res
}

// dedupeKey 定义归因去重身份：同一用户 + 同一开关 + 同一结果变体 + 同一来源规则。
// 短时间内重复评估（结果相同）不会重复上报；用户一旦被分到不同变体（例如规则
// 演进导致），则会正常产生一条新归因。
func dedupeKey(res Result, userID string) string {
	return res.FlagKey + "|" + userID + "|" + res.Variation + "|" + string(res.Source) + "|" + res.HitRuleID
}

// CurrentVersion 返回当前生效配置的版本；无快照时返回 0。
func (e *Engine) CurrentVersion() int {
	if s := e.current.Load(); s != nil {
		return s.version
	}
	return 0
}

// IsStale 返回当前是否在使用降级（最近一次有效）配置。
func (e *Engine) IsStale() bool {
	if s := e.current.Load(); s != nil {
		return s.stale
	}
	return false
}

// LastRefreshError 返回最近一次刷新被拒绝/失败的原因；上次成功刷新后为 nil。
func (e *Engine) LastRefreshError() *ConfigError { return e.lastErr.Load() }

// Close 停止后台刷新并关闭上报器。
func (e *Engine) Close() error {
	e.closeOnce.Do(func() { close(e.stop) })
	e.wg.Wait()
	return e.reporter.Close()
}

func asConfigError(err error) *ConfigError {
	if ce, ok := err.(*ConfigError); ok {
		return ce
	}
	return cfgError(ReasonInvalidField, "", err.Error())
}
