# 配置版本演进与兼容性策略

## 版本

| 版本 | 状态     | 说明                                                        |
|------|----------|-------------------------------------------------------------|
| v1   | 旧版     | 规则用 `order`、variant 权重用整数百分比、开关级 `percent`  |
| v2   | 当前版本 | `priority`、基点权重 `weightBPS`、操作符、`prerequisites`   |

解析后内部统一为 v2 语义：`ParseConfig` 返回的 `Config.Version` 永远是 v2
（v1 在解析阶段完成透明迁移）。v1 与 v2 配置对同一用户给出一致的评估结果，
相关覆盖见 `TestV1_LegacyOrderAndWeight_Migrated`。

## v1 → v2 迁移规则

| v1 写法                        | 迁移结果                                              |
|--------------------------------|-------------------------------------------------------|
| rule `"order": 2`              | `"priority": 2`                                       |
| `"weight": [{"percent":70}]`   | `"variants":[{"weightBPS":7000}]`（percent × 100）    |
| flag 级 `"percent": 100` 且 on | 包装为一条全匹配隐式规则 `<flagKey>-legacy-default`   |
| flag 级 `"percent": 0` 且 on   | **按缺省迁移（100%）**，见下方兼容说明                |
| 未识别的旧字段                 | 忽略（见下）                                          |

### 显式 0 / 缺省的区分只存在于 v2

「`percentage` 缺省 = 全量 100%」与「`percentage: 0` = 命中不投放」的区分是
v2 才引入的语义（规则级字段在 v1 根本不存在）。为保证**旧配置升级后语义
不变**：

- v1 规则没有 `percentage` 字段，迁移后一律为缺省（100%），`PercentageExplicit=false`；
- v1 flag 级 `percent: 0` 在旧引擎里与「不写 percent」无法区分，实际按缺省
  全量处理；迁移时同样映射为**缺省 100%**，绝不会被误判为新的「显式 0 不投放」。
  覆盖：`TestV1_LegacyZeroPercent_StaysFullRollout`。
- 想要「命中不投放」语义必须由 v2 配置显式写出 `"percentage": 0`。

### 变体分桶的兼容范围

- **放量资格哈希保持原样**（`"ffe/v2\n"` 前缀、输入与取模均不变），所以
  存量 v1/v2 配置升级后，每个用户在不在放量窗口内完全不变，比例上调的
  单调性对历史用户继续成立；
- **变体分配哈希（`"ffe/v2/variant\n"`）是新增的独立维度**：它只改变「已在
  窗口内的用户落到哪个变体」。旧引擎用资格桶沿权重曲线选变体，非 100% 放量
  时只有排在权重曲线前面的变体可达——这本身就是本次要修复的缺陷；对历史
  100% 全量、单变体规则（最常见形态），结果仍是唯一变体，没有可观察变化；
  对历史非全量 + 多变体规则，变体分配按新语义重新分布（这是修复目标，而非
  承诺保持的旧行为）。

## 未知字段、非法取值、缺省取值（三类必须分清）

所有配置入口（初始文件、API 推送、后台刷新 payload）都走**同一个**
`flag.ParseConfig`，因此行为在各入口严格一致；绕过解析、直接把构建好的
`Config` 交给引擎时，`Engine.Install` 强制执行同一条校验边界
（`Validate` + `CheckDependencies`），`Snapshot` 仅用于已校验快照：

1. **未知字段 → 静默忽略**（前向兼容）。服务端比客户端新时可以推送额外字段，
   不会打挂老客户端。覆盖：`TestUnknownFields_Ignored`。
2. **未知/缺失版本 → 拒绝** `ErrUnknownVersion`。不猜测版本，避免把 v3
   当 v2 误解。
3. **已知字段的非法取值 → 拒绝**，返回可 `errors.Is` 判别的具体 sentinel：
   权重和不为 10000（`ErrWeightSum`）、百分比越界（`ErrBadPercentage`）、
   非正 priority（`ErrBadPriority`）、未知操作符（`ErrUnknownOperator`）、
   畸形条件（`ErrBadCondition`）、重复 key/rule/variant、非法权重值等。
   所有拒绝都同时 `errors.Is(err, ErrInvalidConfig)`，错误信息包含开关 key /
   规则 id 便于定位。
4. **缺省取值 → 按文档化默认值填充**（见 RULES.md 的默认值表），默认值只在
   语义安全时提供；会导致静默行为变化的字段（如 priority）不给默认值，直接拒绝。

非法 JSON 走 `ErrMalformedJSON`；空 payload / 空 flags 走 `ErrEmptyConfig`。

## 为什么被拒绝的配置不会影响线上

解析与校验全部在新对象上完成，**只有 ParseConfig 成功才会构造出 `Config`**；
`Refresher` 拿到错误时不触碰引擎，旧快照继续服务。依赖环、缺失依赖等语义
错误同样在安装前拦截（`CheckDependencies`）。
