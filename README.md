# Feature Flag Evaluation Engine

一个本地可配置的特性开关评估引擎。规则数据来自可插拔的 `Loader`（模拟配置下发），
由后台 `Refresher` 周期性拉取并原子更新内存快照；调用方在**配置不可用、规则演进、
并发刷新**三种场景下都能拿到稳定且可解释的评估结果。

## 快速开始

```go
cfg, err := flag.ParseConfig(payload)      // 唯一配置入口：解析 + v1 迁移 + 校验
engine := flag.NewEngine()
engine.Snapshot(cfg)                        // 原子安装快照

res := engine.Evaluate(ctx, flag.User{ID: "user-123", Attributes: attrs}, "checkout-redesign")
// res.On / res.Variant / res.Status / res.Source / res.MatchedRule / res.Reason
```

后台刷新 + 失败降级：

```go
ref := flag.NewRefresher(engine, loader)
ref.Start(ctx, 30*time.Second)             // 失败时继续使用上一份有效配置
defer ref.Close()
```

异步归因上报（去重、不阻塞评估）：

```go
rep := flag.NewReporter(sink)
defer rep.Close()
rep.Record(ctx, flag.FromResult(res))      // 永远不返回错误，永不阻塞评估
```

运行演示：

```bash
go run ./cmd/ffdemo
```

## 文档

- [doc/RULES.md](doc/RULES.md) — 规则语义、优先级、分桶稳定性与前置依赖
- [doc/VERSIONS.md](doc/VERSIONS.md) — 配置版本演进、未知/非法/缺省值策略
- [doc/REFRESH_AND_REPORTING.md](doc/REFRESH_AND_REPORTING.md) — 缓存刷新降级与归因去重
- [doc/VALIDATION.md](doc/VALIDATION.md) — 拒绝原因清单与验证方法

## 测试

```bash
go test ./...                              # 全量测试
CGO_ENABLED=1 go test -race -count=1 ./... # 竞态检测
go vet ./...
```
