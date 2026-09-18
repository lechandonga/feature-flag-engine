# Feature Flag Evaluation Engine

一个本地可配置的特性开关评估引擎：用本地 JSON 文件（或任意实现 `ConfigSource`
的数据源）模拟配置下发与缓存刷新，在配置不可用、规则演进与并发评估下都给出
稳定且可解释的结果。

## 能力一览

| 需求 | 实现方式 |
| --- | --- |
| 优先级匹配 + 稳定分桶 | 规则按 `(priority, id)` 排序首条命中；分桶键 `salt|flag|rule|user` 经 FNV-1a 映射到 0..9999 槽位 |
| 结果可解释 | `Result` 带 `Status / Variation / Source / HitRuleID / BucketKey / ConfigVersion / Stale` |
| 前置依赖 | 加载期 DFS 着色检测，区分 `missing_dependency` / `cycle_dependency` / `contradiction` |
| 拒绝隔离 | 坏配置不换入，旧快照继续服务 |
| 多版本配置 | `ParseConfig` 统一入口，v1 自动迁移到内部模型；未知字段忽略，版本过高拒绝 |
| 后台刷新 | `atomic.Pointer` 整体替换不可变快照；刷新失败沿用最近一次有效配置并标记 `stale` |
| 归因上报 | 异步单 worker、有界队列非阻塞入队、窗口内去重；上报失败不影响评估 |

## 快速开始

```go
e := engine.New(engine.WithReporter(reporter))

// 初次加载必须成功一次；之后后台定时刷新
err := e.StartRefresher(ctx,
    &engine.FileSource{Path: "configs/flags-v2.json"}, 30*time.Second)

r := e.Evaluate("checkout_redesign", engine.User{
    ID:         "user-002",
    Attributes: map[string]string{"tier": "free"},
})
fmt.Println(r.Status, r.Variation, r.Source, r.HitRuleID)
```

运行演示：

```bash
go run ./cmd/ffdemo
# 另一个终端修改 configs/flags-v2.json，观察 3 秒内自动刷新
```

无配置时 `Evaluate` 返回 `status=unavailable, reason=config_unavailable`，
引擎不会伪造默认值，由调用方决定兜底策略。

## 目录

- `engine/` — 引擎实现（单包，按职责分文件）
- `configs/flags-v2.json` — 示例配置
- `cmd/ffdemo/` — 本地演示（文件刷新 + 日志归因）
- `docs/SEMANTICS.md` — 规则语义、优先级、版本兼容与验证方法

## 验证

```bash
go test ./...              # 全量单测
CGO_ENABLED=1 go test -race ./...  # 竞态检测（需 gcc）
go vet ./...
gofmt -l .
```

测试覆盖：分桶稳定性（顺序/重排/重启）、依赖缺失/自依赖/环/矛盾、v1/v2 兼容、
未知字段忽略、刷新失败与源故障降级、刷新风暴下并发评估、归因去重与失败隔离。
