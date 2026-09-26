# 拒绝原因清单与验证方法

## 配置拒绝原因（均可 `errors.Is` 判别，且都包装 ErrInvalidConfig）

| Sentinel                      | 触发场景                                    |
|-------------------------------|---------------------------------------------|
| `ErrEmptyConfig`              | 空 payload 或 flags 为空                    |
| `ErrMalformedJSON`            | 不是合法 JSON                               |
| `ErrUnknownVersion`           | version 缺失或不支持                         |
| `ErrMissingField`             | 开关 key / 规则 id / variant key 缺失       |
| `ErrIllegalValue`             | 权重 ≤ 0 等非法取值                         |
| `ErrDuplicateFlagKey`         | 开关 key 重复                               |
| `ErrDuplicateRuleID`          | 同一开关内规则 id 重复                      |
| `ErrDuplicateVariant`         | 同一规则内 variant key 重复                 |
| `ErrWeightSum`                | 权重基点之和 ≠ 10000                        |
| `ErrEmptyVariants`            | 非 off 规则没有任何 variant（直装校验同样触发）|
| `ErrBadPercentage`            | rollout 百分比超出 [0,100]（注意：0 本身合法，表示命中不投放）|
| `ErrBadPriority`              | priority ≤ 0                                |
| `ErrUnknownOperator`          | 未知条件操作符                              |
| `ErrBadCondition`             | 条件缺属性，或 in/not_in 无 values          |
| `ErrMissingDependency`        | prerequisite 指向不存在的开关               |
| `ErrCircularDependency`       | 依赖环（含自依赖），错误信息含环链          |
| `ErrDependencyContradiction`  | 依赖重复，或 prerequisite 静态上永不可 ON   |

## 评估状态（Result.Status）

| Status             | 含义                                       |
|--------------------|--------------------------------------------|
| `on`               | 命中规则且落在放量窗口                     |
| `off`              | 总开关关 / 无命中 / 规则否决 / 窗口外      |
| `flag_not_found`   | key 不在当前配置中                         |
| `unavailable`      | 尚无有效配置，或评估 ctx 已取消            |
| `prereq_not_met`   | 某个前置依赖不为 ON                        |

`Result.Source` 进一步说明取值来源：`rule` / `rule_off` /
`rule_zero_rollout` / `default` / `config_unavailable`；`MatchedRule` 给出
命中规则 id；`Reason` 给出文字决策路径。其中 `rule_zero_rollout` 专指规则
**显式 `percentage: 0`**：规则命中、原因可见，但不投放（区别于缺省=全量）。

## 自动化测试覆盖

```bash
go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
go vet ./...
```

| 场景             | 测试                                                          |
|------------------|---------------------------------------------------------------|
| 分桶确定性       | `TestRolloutValue_Deterministic`                              |
| 评估顺序无关     | `TestSameUserSameBucket_AnyEvaluationOrder`                   |
| 规则重排稳定     | `TestSameUserSameBucket_RuleReorder`                          |
| 优先级语义       | `TestPriority_LowerNumberWins`                                |
| 分布比例         | `TestRolloutDistribution_RoughlyProportional`                 |
| 部分放量全变体可达+权重（5 万用户）| `TestVariants_AllReachableAndWeightedAt50k`     |
| 比例上调单调+变体粘性（5 万用户） | `TestRollout_MonotoneAndVariantSticky_10to50`    |
| 重启/重排判定与变体稳定 | `TestStability_AcrossRestartAndRuleReorder`           |
| 显式 0 vs 缺省   | `TestExplicitZero_VersusOmitted`                             |
| 0/缺省跨入口一致 | `TestExplicitZero_AllIngressesConsistent`                    |
| v1 percent=0 语义不变 | `TestV1_LegacyZeroPercent_StaysFullRollout`            |
| 直装拒绝非法配置+旧配置服务 | `TestInstall_RejectsInvalidHandBuiltConfigs`        |
| 切换/评估并发稳定（-race）| `TestConcurrent_EvaluationAcrossSwaps`                |
| 持续拒装下旧配置稳定服务（-race）| `TestInstall_RejectedSwapUnderConcurrentLoad_KeepsServing` |
| 评估耗时无明显劣化 | `TestEvaluationPerformance_NoRegress`，`BenchmarkEvaluate`  |
| 缺失/循环/矛盾   | `TestMissingDependency_*`, `TestSelfDependency_*`,           |
|                  | `TestCircularDependency_*`, `TestContradiction_*`,            |
|                  | `TestRejectionReasons_AreDistinguishable`                     |
| 拒绝不影响存量   | `TestRejectedConfig_DoesNotAffectExistingEvaluation`          |
| 依赖门控         | `TestPrereqNotMet_GatesFlag`                                  |
| v1 兼容迁移      | `TestV1_LegacyOrderAndWeight_Migrated`                        |
| 未知字段忽略     | `TestUnknownFields_Ignored`                                   |
| 未知版本拒绝     | `TestUnknownVersion_Rejected`                                 |
| 非法值分类拒绝   | `TestIllegalValues_RejectedWithTypedErrors`                   |
| 默认值一致       | `TestDefaults_AppliedConsistently`                            |
| 刷新失败降级     | `TestRefresh_FailureKeepsLastGoodConfig`,                    |
|                  | `TestRefresh_BackgroundLoopAndFailureDegradation`             |
| 相同 payload 跳过| `TestRefresh_UnchangedPayloadSkipped`                         |
| 刷新并发无抖动   | `TestConcurrentEvaluation_NoJitterAcrossRefresh`（-race 下验证）|
| 首次加载前       | `TestEvaluate_BeforeFirstConfig_IsUnavailable`                |
| 上报去重         | `TestReporter_DedupesRepeatedSameOutcome` 等                  |
| 上报故障隔离     | `TestReporter_SinkFailure/SlowSink/FullQueue_*`               |

## 手工验证

```bash
go test -count=1 ./...                                   # 全量单测
CGO_ENABLED=1 go test -race -count=1 ./...               # 并发/切换竞态
go test -run 'TestVariants_AllReachable|TestRollout_Monotone|TestEvaluationPerformance' -v ./...
go test -bench BenchmarkEvaluate -benchmem ./...         # 单次评估耗时
go run ./cmd/ffdemo
```

演示配置见 `configs/example.v2.json`（优先级、条件、30% 三变体放量、否决规则、
前置依赖，以及一个显式 `percentage: 0` 的 `loyalty-points-v3` 待命开关）与
`configs/example.v1.json`（旧版迁移）。可以尝试：

- 把 `beta-30pct` 的比例在 10/20/30/40/50 之间调整，观察已放量用户不收回、
  变体不变（单调性与粘性）；
- 把某条规则改成 `"percentage": 0` 或直接删掉该字段，对比
  `source=rule_zero_rollout`（命中不投放）与缺省全量的差别；
- 把配置改成循环依赖、缺失依赖或权重和错误，观察 ParseConfig 的分类报错；
- 用 `Engine.Install` 直接装入手工构建的自环/坏权重 Config，观察拒装后
  上一份配置继续服务；或让 Loader 返回错误，观察评估继续命中旧快照。
