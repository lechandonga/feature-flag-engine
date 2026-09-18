# 缓存刷新、降级与归因上报

## 后台刷新模型

```
Loader (可插拔: 文件/HTTP/...)
   │  Load(ctx) -> raw bytes
   ▼
ParseConfig  ──失败──▶ 记录 stats，保留旧快照（引擎完全不受影响）
   │ 成功
   ▼
Engine.Snapshot(cfg)  // atomic.Pointer.Store，整份配置原子切换
```

- 活跃配置保存在 `atomic.Pointer[Config]` 中。刷新时新配置先**完整构建并校验**，
  最后一次原子 Store 完成切换：进行中的评估继续读旧快照，新评估读新快照，
  不存在中间状态，因此并发评估不会出现结果抖动、短暂失效或空结果。
- `Config` 本身不可变（安装后没有任何写路径），无需加读锁。
- 刷新周期由 `Refresher.Start(ctx, interval)` 驱动；启动时立即刷新一次，
  不必等一个 interval 才有配置。
- 并发安全：`refreshMu` 串行化刷新周期，手动 `RefreshOnce` 与后台循环不会重叠。
- **字节级相同的 payload 会跳过安装**（减少无谓的快照切换与统计噪声）。

## 首次加载前怎么办？

引擎在尚未获得任何有效配置时返回结构化结果：
`status=unavailable, source=config_unavailable`，而不是 nil / panic / 空值。
调用方可据此选择自己的硬编码兜底。

## 失败降级矩阵

| 失败点               | 行为                                           |
|----------------------|------------------------------------------------|
| Loader 返回 error    | 记录 LastError，旧快照继续服务                 |
| payload 非法 JSON    | 同上                                           |
| 语义校验失败（环等） | 同上                                           |
| 一直失败、从未成功   | 评估返回 unavailable 结构化结果                |
| 刷新恢复成功         | 原子切换到新快照，Stats 更新                   |

`Refresher.Stats()` 给出 `LastOK / LastError / LastVersion / UpdatedAt /
Successes / Failures`，可接入监控。

## 归因上报

`Reporter` 把评估结果异步投递给可插拔 `Sink`：

- **去重**：key 为 `(flagKey, userID)`；当结果签名
  （status + variant + source + matchedRule + configVersion）与上次相同且
  在去重时间窗（默认 5 分钟，`WithDedupeWindow` 可调）内时抑制上报。
  同一用户同一开关短时间内重复评估**只上报一次**；结果变化立即上报。
- **完全异步、永不阻塞评估**：`Record` 只往有界 channel 投递后立即返回，
  没有 error 返回值。
- **sink 故障隔离**：每次投递带 3s 超时，失败后立即重试一次，仍失败只累计
  `failed` 计数；sink 慢或挂死不会传导到评估链路。
- **背压**：队列满（默认 1024，`WithQueueSize` 可调）时丢弃最旧压力之外的
  新事件并累计 `dropped`，而不是阻塞评估（评估可用性优先于归因完整性）。
- 关闭时 worker 立即停止排空，避免慢 sink 拖住进程退出；可通过
  `Stats()` 观测 sent/failed/dropped。
