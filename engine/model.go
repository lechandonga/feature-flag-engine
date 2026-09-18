package engine

// 内部领域模型。无论配置来自哪个 schema 版本，解析后都统一归一化为本包的
// 内部结构，使评估链路不感知版本差异。

// Operator 是规则匹配支持的操作符。
type Operator string

const (
	OpIn          Operator = "in"
	OpNotIn       Operator = "not_in"
	OpStartsWith  Operator = "starts_with"
	OpGreaterThan Operator = "gt"
	OpLessThan    Operator = "lt"
	OpEqual       Operator = "eq"
)

// Condition 描述针对用户某个属性的单个匹配条件。
type Condition struct {
	Attribute string
	Operator  Operator
	Values    []string
}

// Rule 是一条目标规则；优先级数字越小越先匹配。
type Rule struct {
	ID       string
	Priority int
	Match    []Condition
	// Variation 非空时表示命中即固定取该变体（targeting 规则）。
	Variation string
	// Bucket 非空时表示命中后按稳定哈希在 Variations 中分桶。
	Bucket *BucketSpec
}

// BucketSpec 描述基于百分比的稳定分桶。
type BucketSpec struct {
	// Variations 为分桶槽位，权重由 Weights 给出（与 Variations 等长，总和必须为 10000）。
	Variations []string
	Weights    []int
	// Salt 参与哈希计算，默认使用 flag key，使不同开关的分桶互不影响。
	Salt string
}

// Flag 是单个特性开关归一化后的定义。
type Flag struct {
	Key              string
	DefaultVariation string
	Rules            []Rule
	// Prerequisites 为前置依赖：本开关仅当这些开关求值为指定变体时才可正常分桶。
	Prerequisites []Prerequisite
	// Variations 声明该开关允许输出的全部变体（含默认值），用于矛盾检测。
	Variations []string
}

// Prerequisite 声明对另一个开关求值结果的要求。
type Prerequisite struct {
	Flag      string
	Variation string
}

// Config 是一份完整归一化配置。
type Config struct {
	// Version 为归一化后的 schema 版本号（如 1、2）。
	Version int
	// Source 记录配置来源标识（如文件路径 / 下发版本号），便于归因展示。
	Source string
	Flags  map[string]*Flag
}

// ValueSource 说明评估值的来源，供结果解释与上报使用。
type ValueSource string

const (
	// SourceRule 取值来自命中规则的固定变体。
	SourceRule ValueSource = "rule"
	// SourceBucket 取值来自命中规则的稳定分桶。
	SourceBucket ValueSource = "bucket"
	// SourceDefault 取值来自开关默认变体（无规则命中）。
	SourceDefault ValueSource = "default"
	// SourcePrerequisiteDefault 因前置依赖不满足而取默认变体。
	SourcePrerequisiteDefault ValueSource = "prerequisite_default"
)

// EvalStatus 是评估结果的状态。
type EvalStatus string

const (
	// StatusOK 正常评估（含前置满足后的默认值回退）。
	StatusOK EvalStatus = "ok"
	// StatusPrerequisiteNotMet 前置依赖不满足，返回默认变体。
	StatusPrerequisiteNotMet EvalStatus = "prerequisite_not_met"
	// StatusFlagNotFound 请求的开关不存在。
	StatusFlagNotFound EvalStatus = "flag_not_found"
	// StatusUnavailable 配置整体不可用（无任何有效快照）。
	StatusUnavailable EvalStatus = "unavailable"
)

// User 是评估输入的用户上下文。属性键统一为字符串。
type User struct {
	ID         string
	Attributes map[string]string
}

// Attr 取用户属性；nil 安全。
func (u User) Attr(key string) (string, bool) {
	if u.Attributes == nil {
		return "", false
	}
	v, ok := u.Attributes[key]
	return v, ok
}

// Result 是一次评估的可解释结果。
type Result struct {
	Status    EvalStatus
	FlagKey   string
	Variation string
	Source    ValueSource
	// HitRuleID 为命中的规则 ID；无命中时为空。
	HitRuleID string
	// BucketKey 为实际参与哈希的字符串（仅 Source=SourceBucket 时有意义）。
	BucketKey string
	// ConfigVersion 为本次评估使用的配置版本。
	ConfigVersion int
	// Stale 为 true 表示当前配置是刷新失败后沿用的最近一次有效快照。
	Stale bool
	// Reason 在非 OK 状态时给出机器可读的补充原因。
	Reason string
}

// Attribution 是一次需要上报的归因记录。
type Attribution struct {
	FlagKey       string
	UserID        string
	Variation     string
	Source        ValueSource
	HitRuleID     string
	ConfigVersion int
	DedupeKey     string
}
