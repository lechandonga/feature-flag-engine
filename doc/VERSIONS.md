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
| flag 级 `"percent": N>0` 且 on | 包装为一条全匹配隐式规则 `<flagKey>-legacy-default`   |
| flag 级 `"percent": 0` 且 on   | 同上，但 percentage 置为缺省（见下「兼容范围」）      |
| flag 级 `"percent"` 越界       | 拒绝 `ErrBadPercentage`（见下「越界取值」）           |
| 未识别的旧字段                 | 忽略（见下）                                          |

### v1 越界取值的处理

v1 的开关级 `percent` 合法区间是 [0,100]，旧版本对越界值（负数、大于 100）
判为非法。升级路径保持这一行为：`percent` 越界的 v1 配置在迁移阶段即被
`ParseConfig` 拒绝（`ErrBadPercentage`），**不会**被悄悄当成缺省的全量
投放放出去。覆盖：`TestV1_PercentOutOfRange_Rejected`；合法值（含 0）的
迁移行为不变，见 `TestV1_PercentInRange_PreservesLegacySemantics`。

## v1/v2 兼容范围：显式 0 的边界

v2 中 `"percentage": 0` 是**显式 0**：命中规则但不投放（区别于缺省 = 100）。
而 v1 的历史行为里 `percent: 0` 与缺省同义（全量投放）。为保证**旧版本配置
升级后语义不变**，v1 的 `percent: 0` 迁移为「缺省 percentage」而非显式 0；
v1 的 `percent: N>0` 迁移为显式 N。也就是说：

- v1 配置：任何写法下的评估结果与迁移前完全一致；
- v2 配置：显式 0 与缺省的区分在所有入口（本地文件、API 推送、后台刷新）
  一致生效；
- 灰度放量请使用 v2 schema 表达显式 0。

## 未知字段、非法取值、缺省取值（三类必须分清）

所有配置入口（初始文件、API 推送、后台刷新 payload）都走**同一个**
`flag.ParseConfig`，因此行为在各入口严格一致：

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
   `percentage` 用指针字段解析，显式 `0` 与「未提供」严格区分。

非法 JSON 走 `ErrMalformedJSON`；空 payload / 空 flags 走 `ErrEmptyConfig`。

## 绕过解析的直接切换入口

调用方也可以不经过 JSON 解析，把手工构建的 `*Config` 交给
`Engine.Install(cfg)`。该入口执行与 `ParseConfig` **同一套**校验
（`Config.Validate` 结构校验 + `Config.CheckDependencies` 依赖校验），
两条入口的校验边界完全一致，包括：

- 条件校验：未知操作符（`ErrUnknownOperator`）、空属性、in/not_in 缺少
  候选值（`ErrBadCondition`）同样被拒绝——不允许出现「直装能装、解析会拒」
  的规则（这类规则装进去后要么永不命中、要么反向全命中，与解析入口表现
  不一致）；
- 标识一致性：开关在 `Config.Flags` 表中的键必须与其声明的 `Flag.Key`
  一致（`ErrFlagKeyMismatch`）；
- 显式 0 与缺省：直装时 `Rule.Percentage` 是标量，0 即显式 0（命中但不
  投放），与解析入口的 `"percentage": 0` 语义一致；缺省 100 的语义由
  解析入口在 normalize 阶段落实，直装方需自行给出目标值。

拒绝时返回同样可分类的错误，旧配置继续服务；并发评估在切换/拒装过程中
不阻塞、不抖动、不返回空结果。两入口判定一致性由 `TestInstall_ParseParity`
覆盖。

## 为什么被拒绝的配置不会影响线上

解析与校验全部在新对象上完成，**只有 ParseConfig 成功才会构造出 `Config`**；
`Refresher` 拿到错误时不触碰引擎，旧快照继续服务。依赖环、缺失依赖等语义
错误同样在安装前拦截（`CheckDependencies`）。
