# 规则语义与验证说明

本文档说明配置格式、匹配/分桶语义、依赖规则、版本演进策略，以及如何验证这些保证。

## 1. 配置模型（v2，当前版本）

```json
{
  "version": 2,
  "flags": {
    "flag-key": {
      "default_variation": "off",
      "variations": ["off", "on"],
      "rules": [
        {
          "id": "staff-only",
          "priority": 1,
          "match": [
            {"attribute": "tier", "operator": "eq", "values": ["staff"]}
          ],
          "variation": "on"
        },
        {
          "id": "rollout",
          "priority": 10,
          "bucket": {
            "variations": ["off", "on"],
            "weights": [9000, 1000],
            "salt": "optional-hash-salt"
          }
        }
      ],
      "prerequisites": [
        {"flag": "another-flag", "variation": "on"}
      ]
    }
  }
}
```

### 规则字段

- `id`：开关内必填且唯一（重复 = `duplicate_definition`）。
- `priority`：数字越小越先评估；**缺省为 100**；负数拒绝。
- `match`：条件数组，AND 语义；空数组/缺省表示无条件命中所有人。
- 规则必须且只能设置 `variation`（固定变体）或 `bucket`（分桶）之一，
  同时设置 = `contradiction`，都不设置 = `invalid_field`。

### 条件操作符

| operator | 语义 | 属性缺失时 |
| --- | --- | --- |
| `in` | 属性值在 values 中 | 不匹配 |
| `not_in` | 属性值不在 values 中 | **匹配** |
| `starts_with` | 属性值以任一前缀开头 | 不匹配 |
| `eq` | 等于 values[0] | 不匹配 |
| `gt` / `lt` | 双方按浮点解析后比较；解析失败 | 不匹配 |
| （缺省） | 等价于 `in` | 同 `in` |

`values` 必须非空；未知操作符 = `invalid_field`。

### 分桶

- 权重单位为万分比：`weights` 与 `variations` 等长，每个值 ∈ [0,10000]，
  总和必须恰好为 10000，否则拒绝。
- 哈希：`FNV-1a-64(salt|flagKey|ruleId|userId) mod 10000`。
  - `salt` 缺省取 flag key；
  - 输入与**配置书写顺序、权重、变体排列顺序无关**，因此规则重排不改变槽位；
  - 槽位→变体按变体声明顺序累加权重映射，建议只在尾部新增变体/权重，
    把权重调整对既有用户的影响降到最低。
- 分桶结果 `Result.BucketKey` 回传实际哈希输入，便于排查。

### 评估顺序

1. 前置依赖递归求值（深度上限 64，运行期环兜底降级，不 panic）；
   任一前置结果不等于要求变体 ⇒ `status=prerequisite_not_met`，
   取本开关默认变体，`source=prerequisite_default`。
2. 规则按 `(priority ASC, id ASC)` 排序后首条命中：
   - 固定变体 ⇒ `source=rule`；
   - 分桶 ⇒ `source=bucket`；
3. 无命中 ⇒ 默认变体，`source=default`。
4. 开关不存在 ⇒ `status=flag_not_found`；无任何配置快照 ⇒
   `status=unavailable, reason=config_unavailable`。

## 2. 前置依赖与拒绝原因

所有跨开关检查都在**加载/刷新时**完成，拒绝原因机器可区分：

| reason | 触发条件 |
| --- | --- |
| `missing_dependency` | `prerequisites[].flag` 未在本配置定义 |
| `cycle_dependency` | 依赖图存在环（含 `flag` 依赖自身） |
| `contradiction` | 要求被依赖开关产出它永远不会产生的变体；或一条规则同时给 variation 和 bucket |
| `duplicate_definition` | flag key / rule id 重复 |
| `invalid_field` | 类型、枚举、权重和、必填项等不合法 |
| `unsupported_version` | `version` 高于本引擎支持的最高版本 |

检测顺序固定：单字段 → 缺失依赖/自依赖 → 环（DFS 灰节点）→ 矛盾，
因此同一份坏配置每次报错稳定。**被拒绝的配置不会换入**，线上行为不变。

## 3. 版本演进与兼容策略

| 情形 | 策略 |
| --- | --- |
| `version` 缺省 | 按 **v1** 处理（最早的配置形态） |
| 未知字段（任意层级） | **忽略**（前向兼容：新下发字段不打挂旧引擎） |
| `version > 2` | 拒绝：`unsupported_version`（拒绝按错误语义解释新配置） |
| 非法枚举/非法权重/未知操作符 | 拒绝：`invalid_field`（不静默猜测） |
| 可安全补的缺省值 | `priority=100`、`operator=in`、`salt=flagKey`、v1 `default/off` |

### v1 → 内部模型迁移

v1 结构：`on / variation / default / whitelist{uid: variation} / allowed_values`。

- `whitelist` 的每个 uid 转为 `priority=10` 的 `uid in [...]` 固定变体规则
  （Go map 遍历顺序不影响结果：最终按 priority+id 排序）；
- `on=true && variation!= ""` 追加一条 `priority=100` 的无条件兜底规则；
- `default` 缺省为 `"off"`；未声明的取值自动补进变体集合。
- 因此 v1 配置在 v2 引擎上的用户分群保持其原有语义，并可与 v2 开关在
  同一份配置中热升级（测试 `TestRefreshSuccessAtomicallySwaps` 即 v1→v2）。

## 4. 刷新与并发语义

- 引擎内部只持有 `atomic.Pointer[snapshot]`；快照构建（解析+校验）全部在
  旧快照之外完成，成功后**一次原子写入**换入。评估只读快照、无锁。
- 刷新失败（源故障或配置被拒）：保留最近有效快照，标记 `Result.Stale=true`，
  错误可经 `LastRefreshError()` 读取；下次成功自动清除 stale。
- 从未成功加载时：评估返回 `unavailable`，不返回空值/猜测默认值。
- 并发验证见 `TestConcurrentEvaluationDuringRefresh` 与
  `TestStableUnderConcurrentRefresh`（建议以 `-race` 运行）。

## 5. 归因上报语义

- 仅对存在的开关、有效评估（`ok` / `prerequisite_not_met`）上报。
- 去重键：`flag|user|variation|source|hitRule`；默认窗口 5 分钟。
  用户结果发生变化（规则演进后被分到另一变体）会产生新归因。
- 上报完全异步：有界队列满即丢弃计数，`Send` 失败只计数，
  评估链路不等待、不感知上报结果。
- 指标：`SentCount()` / `DroppedCount()`（含去重、队列满、发送失败）。

## 6. 验证方法

```bash
go test ./...
CGO_ENABLED=1 go test -race ./...
```

关键测试与需求的对应关系：

| 测试 | 覆盖的保证 |
| --- | --- |
| `TestHashToSlotStableAcrossCalls` / `TestBucketKeyDeterminism` | 哈希纯函数、跨开关/规则/用户隔离 |
| `TestStableAcrossEvaluationOrder` / `...RuleReorder` / `...Restart` | 任意评估顺序、规则重排、进程重建后分组一致 |
| `TestPriorityTargetingWinsOverRollout` | 小优先级数字先匹配，定向规则压过分桶 |
| `TestMissingDependencyRejected` / `Self` / `Cycle` / `Contradiction` | 依赖问题可区分拒绝 |
| `TestRejectedConfigDoesNotMutateOldOne` | 坏配置不影响既有评估 |
| `TestParseV1Defaults/OnAndWhitelist` / `TestParseV2Basic` / `TestParseUnknownFieldsIgnored` / `TestParseUnknownVersionRejected` | 版本兼容与未知字段策略 |
| `TestRefreshFailureKeepsLastGoodConfig` | 坏配置 + 源故障双降级 + 恢复清除 stale |
| `TestConcurrentEvaluationDuringRefresh` / `TestStableUnderConcurrentRefresh` | 刷新风暴中无空结果/无抖动 |
| `TestReporterDedupWithinWindow` / `QueueFullNeverBlocks` / `FailureDoesNotAffectEvaluation` | 去重、背压不阻塞、上报失败隔离 |
