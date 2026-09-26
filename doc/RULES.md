# 规则语义、优先级与分桶

## 评估决策路径（按顺序短路）

给定用户与开关 key，`Engine.Evaluate` 依次判断：

1. **无可用配置**（进程启动后尚无一次成功刷新）→ `status=unavailable`，
   `source=config_unavailable`，返回 OFF，**不返回空值**，`reason` 说明原因。
2. **开关不存在** → `status=flag_not_found`，OFF，reason 带当前配置版本。
3. **前置依赖**：按声明顺序递归评估每个 prerequisite；任意一个不为 ON
   → `status=prereq_not_met`，OFF，reason 指出具体哪个依赖未满足。
4. **总开关 off** 且无规则命中（实际上总开关 off 时直接判定）→
   `status=off`，`source=default`。
5. **规则匹配**：按 `priority` 升序（数字越小优先级越高）取第一条所有条件
   都满足的规则；priority 相同时按规则 id 字典序打破平局，得到**全序**，
   因此 JSON 里规则的物理排列顺序不影响结果。
6. **命中规则后的四种结果**：
   - 规则 `"off": true` → 熔断/否决，`source=rule_off`；
   - 规则**显式写了** `"percentage": 0` → 命中但不投放，
     `source=rule_zero_rollout`，`matched_rule` 与 reason 仍完整可见；
   - 用户放量分桶值 ≥ rollout 阈值（`percentage * 100`）→ 未放量，
     `source=default`，但 `matched_rule` 仍记录命中的规则；
   - 在放量窗口内 → ON，按**独立的变体分桶**沿权重曲线选 variant，
     `source=rule`。

每个 `Result` 都带有 `Reason`（决策路径的文字解释，含命中规则、分桶值与
阈值）和 `MatchedRule`（取值来源），做到**可解释**。

## 条件操作符

| operator   | 含义                              | 要求            |
|------------|-----------------------------------|-----------------|
| `eq`       | 属性等于 `value`                  | 一个值          |
| `ne`       | 属性不等于 `value`                | 一个值          |
| `in`       | 属性属于 `values`                 | 至少一个值      |
| `not_in`   | 属性不属于 `values`               | 至少一个值      |
| `contains` | 属性字符串包含子串                | 一个值          |
| `starts_with` | 属性字符串前缀匹配              | 一个值          |

同一条规则内多个条件为 **AND**；`in`/`not_in` 的空值列表按非法配置拒绝。

## 确定性分桶：放量资格与变体分配是两个独立哈希

放量过程包含**两个相互独立的问题**，分别用两个独立、确定的哈希回答：

```
# 1) 放量资格：该用户当前在不在放量窗口内？
rolloutBucket = sha256("ffe/v2\n"        + flagKey + "\n" + salt + "\n" + userID) mod 10000
# 2) 变体分配：在窗口内的用户落到哪个变体？
variantBucket = sha256("ffe/v2/variant\n" + flagKey + "\n" + salt + "\n" + userID) mod 10000
```

- 两个哈希**域前缀不同、互不相关**。若共用一个桶，部分放量（如 30%）时
  窗口只覆盖桶空间前 3000 个值，累计权重曲线上排前面的变体会吃掉全部流量，
  后面的变体永远不可达；独立哈希后，**进入放量范围的用户按声明权重覆盖
  全部变体**（5 万用户量级的覆盖与比例验证见
  `TestVariants_AllReachableAndWeightedAt50k`）。
- 两个哈希的输入都**只包含**开关 key、用户 ID 和 salt（当前留空），刻意
  排除规则 id、规则排列、时间戳、配置版本与进程状态；
- 所以同一用户：
  - 无论评估多少次、评估顺序如何，放量判定与变体都一致；
  - **规则重排**（物理顺序变化、priority 调整但同一规则仍命中）不影响任一桶；
  - **进程重启**、换机器，结果一致（纯数据哈希，无内存状态参与）；
- 10000 个桶 = 0.01% 粒度；`percentage: 50` 对应窗口 `[0,5000)`；
- variant 选择沿累计权重曲线定位 **variantBucket**，权重以基点（bps）
  表示，必须**精确求和 10000**，否则配置被拒绝（避免隐式归一化掩盖配置错误）。
- 放量资格哈希的编码（`"ffe/v2\n"` 前缀）**永不改变**：改变它会重排所有
  用户的放量资格，直接破坏单调性承诺；变体哈希是新增加密独立维度，不影响
  存量用户的资格判定。

> 稳定性的边界：如果运营把某用户的实际命中规则从一条变成另一条（改条件或
> priority 使命中规则变化），variant 可能改变——这是配置意图，不是抖动。
> 抖动特指「配置语义不变、仅物理排列/时间/评估顺序不同」导致的结果漂移。

## 微调放量比例时用户会怎样？（单调放量 + 变体粘性）

两个桶的值本身都永不变。把 `percentage` 从 10 提到 20、再到 50：

- **资格单调**：每一步只把窗口 `[旧阈值, 新阈值)` 内的用户从 OFF 变成 ON，
  已经放量的用户永远不会被收回（阈值下调才会收回对应区间，那是显式缩容）；
- **变体粘性**：变体桶与比例无关，已放量用户在比例上调前后算出的
  variantBucket 完全相同，因此变体保持不变；新放量的用户独立按权重落桶。
- 覆盖：`TestRollout_MonotoneAndVariantSticky_10to50`（5 万用户，10→50 逐步验证）。

## 显式 `percentage: 0` 与「不写这个字段」的区别

这是两种**刻意不同**的语义，在所有配置入口（本地文件、API 直推、后台刷新、
`Engine.Install` 直装）下严格一致：

| 写法                        | 含义                                                        | 命中结果 |
|-----------------------------|-------------------------------------------------------------|----------|
| 不提供 `percentage`         | 沿用全量默认 = 100%，命中即投放                             | 正常放量 |
| `"percentage": 0`           | **命中也不投放**（暂停/待命规则）                           | OFF，`source=rule_zero_rollout`，`matched_rule` 仍有值，reason 说明原因 |
| `"percentage": 0.0001` 等   | 正常的非零比例，按窗口放量                                  | 正常放量 |

解析层用 `*float64` 区分「缺省（nil → 100）」与「显式 0（保留为 0）」，
归一化后的 `Rule.PercentageExplicit` 记录该字段是否由 payload 显式提供。
覆盖：`TestExplicitZero_VersusOmitted`、`TestExplicitZero_AllIngressesConsistent`。

## 前置依赖

- `prerequisites` 按**声明顺序**递归评估（短路），全部 ON 本开关才可能 ON；
- 配置加载时构建依赖图并做强校验，三类问题在**配置入口**被拒绝，错误可区分：
  1. `ErrMissingDependency` — 依赖了不存在的开关；
  2. `ErrCircularDependency` — 依赖图有环（自依赖是 1 节点环，错误信息给出环链）；
  3. `ErrDependencyContradiction` — 自相矛盾：依赖重复声明，或 prerequisite
     在静态上永不可能 ON（该开关 off 且无规则）；
- **被拒绝的配置永不安装**：ParseConfig 返回错误，Refresher 不调用安装，
  已有评估行为完全不受影响。
- **直装入口同等严格**：绕过解析、用代码直接构建 `Config` 时，必须调用
  `Engine.Install(cfg)`（而不是 `Snapshot`）。Install 在原子切换前执行与
  ParseConfig **完全相同**的边界：`Config.Validate()`（结构不变量）+
  `CheckDependencies()`（依赖图）。自环、成环、缺失依赖、静态不自洽，以及
  priority ≤ 0、权重和 ≠ 10000、未知操作符等任何结构问题都会被拒装。
  拒装时最近一次有效配置继续服务；校验通过后才做一次原子 Store，
  因此并发评估不会崩溃、阻塞、抖动或读到半成品/空结果。
  覆盖：`TestInstall_RejectsInvalidHandBuiltConfigs`、
  `TestConcurrent_EvaluationAcrossSwaps`、
  `TestInstall_RejectedSwapUnderConcurrentLoad_KeepsServing`（-race 下验证）。

## 默认值（显式且在所有入口一致）

| 缺省情况                    | 行为                                          |
|-----------------------------|-----------------------------------------------|
| 顶层无 flags / 空 payload   | 拒绝（`ErrEmptyConfig`）                      |
| 开关 `on:true` 且无规则     | 注入隐式规则：100% 放量、单一 `on` variant    |
| 开关 `on:false`             | 恒 OFF，不注入任何规则                        |
| 规则缺省 `percentage`       | 100（命中即全量）；**显式 0 则命中不投放**（见上表）       |
| 规则无 variants 且非 off    | 单一隐式 `on` variant，权重 10000 bps         |
| 规则无 conditions           | 匹配所有用户                                  |
| 规则缺省/非正 `priority`    | **拒绝**（静默排序变化太危险，不做默认）      |
