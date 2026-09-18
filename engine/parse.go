package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// MaxSupportedVersion 是本引擎支持的最高配置 schema 版本。
// 更高版本的配置会被拒绝（原因 ReasonUnsupportedVersion），避免旧引擎按错误
// 语义解释新配置。
const MaxSupportedVersion = 2

// 版本缺失时（version 字段缺省）按 v1 处理：v1 是最早的配置形态，
// 这样老配置不加 version 字段也能继续被正确解析。
const defaultVersion = 1

// ------------- 各版本的原始 schema -------------

type rawEnvelope struct {
	Version int                        `json:"version"`
	Flags   map[string]json.RawMessage `json:"flags"`
}

// rawFlagV1: v1 只有固定变体规则与默认值，无条件结构、无分桶、无依赖。
type rawFlagV1 struct {
	On            bool              `json:"on"`
	Variation     string            `json:"variation"`
	Default       string            `json:"default"`
	Whitelist     map[string]string `json:"whitelist"`
	AllowedValues []string          `json:"allowed_values"`
}

// rawFlagV2: v2 引入显式规则、条件匹配、百分比分桶与前置依赖。
type rawFlagV2 struct {
	DefaultVariation string        `json:"default_variation"`
	Variations       []string      `json:"variations"`
	Rules            []rawRuleV2   `json:"rules"`
	Prerequisites    []rawPrereqV2 `json:"prerequisites"`
}

type rawRuleV2 struct {
	ID        string           `json:"id"`
	Priority  *int             `json:"priority"`
	Match     []rawConditionV2 `json:"match"`
	Variation string           `json:"variation"`
	Bucket    *rawBucketV2     `json:"bucket"`
}

type rawConditionV2 struct {
	Attribute string   `json:"attribute"`
	Operator  string   `json:"operator"`
	Values    []string `json:"values"`
}

type rawBucketV2 struct {
	Variations []string `json:"variations"`
	Weights    []int    `json:"weights"`
	Salt       string   `json:"salt"`
}

type rawPrereqV2 struct {
	Flag      string `json:"flag"`
	Variation string `json:"variation"`
}

// ParseConfig 将原始 JSON 配置解析、校验并归一化为内部 Config。
// 所有版本共用这一入口：未知版本、非法取值、缺省取值、依赖图问题都会在这里
// 被拒绝，调用方拿到的 Config 一定是自洽的。
//
// 未知字段策略（所有版本一致）：直接忽略（encoding/json 默认行为），
// 保证新版本下发的附加字段不会打挂旧引擎。
func ParseConfig(data []byte, source string) (*Config, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, cfgError(ReasonInvalidField, "", "empty config payload")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	var env rawEnvelope
	if err := dec.Decode(&env); err != nil {
		return nil, cfgError(ReasonInvalidField, "", "invalid JSON: "+err.Error())
	}

	version := env.Version
	if version == 0 {
		version = defaultVersion
	}
	if version < 1 || version > MaxSupportedVersion {
		return nil, cfgError(ReasonUnsupportedVersion, "version",
			fmt.Sprintf("version %d is not supported (want 1..%d)", version, MaxSupportedVersion))
	}
	if len(env.Flags) == 0 {
		return nil, cfgError(ReasonInvalidField, "flags", "at least one flag is required")
	}

	cfg := &Config{
		Version: version,
		Source:  source,
		Flags:   make(map[string]*Flag, len(env.Flags)),
	}

	for key, raw := range env.Flags {
		p := "flags." + key
		if key == "" {
			return nil, cfgError(ReasonInvalidField, "flags", "flag key must not be empty")
		}
		f, err := parseFlag(version, key, raw)
		if err != nil {
			return nil, wrapPath(p, err)
		}
		if _, dup := cfg.Flags[key]; dup {
			return nil, cfgError(ReasonDuplicateDefinition, p, "duplicate flag definition")
		}
		cfg.Flags[key] = f
	}

	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func parseFlag(version int, key string, raw json.RawMessage) (*Flag, error) {
	switch version {
	case 1:
		var rf rawFlagV1
		if err := json.Unmarshal(raw, &rf); err != nil {
			return nil, cfgError(ReasonInvalidField, "", "invalid flag: "+err.Error())
		}
		return normalizeV1(key, &rf), nil
	default:
		var rf rawFlagV2
		if err := json.Unmarshal(raw, &rf); err != nil {
			return nil, cfgError(ReasonInvalidField, "", "invalid flag: "+err.Error())
		}
		return normalizeV2(key, &rf)
	}
}

// normalizeV1 把 v1 配置迁移到统一模型：
//   - on=false 或缺省 -> 默认变体为 "off"；on=true 且无 variation -> "on"；
//   - variation 为开关全开时的固定值，转成一条最低优先级（数字最大）的兜底规则；
//   - whitelist 转成优先级 10 的 uid in 规则；
//   - allowed_values 作为变体声明；缺省时用已知取值补全。
func normalizeV1(key string, rf *rawFlagV1) *Flag {
	f := &Flag{Key: key}

	// default 缺省统一为 "off"；on 仅影响是否追加全开兜底规则。
	if rf.Default == "" {
		rf.Default = "off"
	}
	f.DefaultVariation = rf.Default

	allowed := append([]string(nil), rf.AllowedValues...)

	const uidRulePriority = 10

	// map 遍历顺序随机：先按 uid 排序再生成规则，保证同一份 v1 配置在任意进程
	// 迁移出的规则 ID 与顺序完全确定（可解释性与归因稳定性）。
	uids := make([]string, 0, len(rf.Whitelist))
	for uid := range rf.Whitelist {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for idx, uid := range uids {
		variation := rf.Whitelist[uid]
		f.Rules = append(f.Rules, Rule{
			ID:        fmt.Sprintf("v1-whitelist-%d", idx),
			Priority:  uidRulePriority,
			Match:     []Condition{{Attribute: "uid", Operator: OpIn, Values: []string{uid}}},
			Variation: variation,
		})
		allowed = appendIfMissing(allowed, variation)
	}

	if rf.On && rf.Variation != "" {
		f.Rules = append(f.Rules, Rule{
			ID:        "v1-on",
			Priority:  100,
			Match:     nil, // 无条件：命中所有人
			Variation: rf.Variation,
		})
		allowed = appendIfMissing(allowed, rf.Variation)
	}
	allowed = appendIfMissing(allowed, rf.Default)

	f.Variations = allowed
	return f
}

func normalizeV2(key string, rf *rawFlagV2) (*Flag, error) {
	f := &Flag{Key: key}

	if rf.DefaultVariation == "" {
		return nil, cfgError(ReasonInvalidField, "default_variation", "default_variation is required")
	}
	f.DefaultVariation = rf.DefaultVariation
	f.Variations = append([]string(nil), rf.Variations...)

	seenRule := make(map[string]bool, len(rf.Rules))
	for i := range rf.Rules {
		rr := &rf.Rules[i]
		p := fmt.Sprintf("rules[%d]", i)
		if rr.ID == "" {
			return nil, cfgError(ReasonInvalidField, p+".id", "rule id is required")
		}
		if seenRule[rr.ID] {
			return nil, cfgError(ReasonDuplicateDefinition, p, "duplicate rule id: "+rr.ID)
		}
		seenRule[rr.ID] = true

		r := Rule{ID: rr.ID}
		// priority 缺省为 100；非法（负数）取值显式拒绝。
		if rr.Priority == nil {
			r.Priority = 100
		} else {
			if *rr.Priority < 0 {
				return nil, cfgError(ReasonInvalidField, p+".priority", "priority must be >= 0")
			}
			r.Priority = *rr.Priority
		}

		if rr.Variation != "" && rr.Bucket != nil {
			return nil, cfgError(ReasonContradiction, p,
				"rule must not set both fixed variation and bucket")
		}
		if rr.Variation == "" && rr.Bucket == nil {
			return nil, cfgError(ReasonInvalidField, p,
				"rule must set either variation or bucket")
		}

		for j := range rr.Match {
			c, err := normalizeCondition(&rr.Match[j], fmt.Sprintf("%s.match[%d]", p, j))
			if err != nil {
				return nil, err
			}
			r.Match = append(r.Match, c)
		}

		if rr.Bucket != nil {
			b, err := normalizeBucket(rr.Bucket, p+".bucket")
			if err != nil {
				return nil, err
			}
			r.Bucket = b
		} else {
			r.Variation = rr.Variation
		}
		f.Rules = append(f.Rules, r)
	}

	for i := range rf.Prerequisites {
		pp := &rf.Prerequisites[i]
		p := fmt.Sprintf("prerequisites[%d]", i)
		if pp.Flag == "" {
			return nil, cfgError(ReasonInvalidField, p+".flag", "prerequisite flag is required")
		}
		if pp.Variation == "" {
			return nil, cfgError(ReasonInvalidField, p+".variation", "prerequisite variation is required")
		}
		f.Prerequisites = append(f.Prerequisites, Prerequisite{Flag: pp.Flag, Variation: pp.Variation})
	}

	appendIfMissing(f.Variations, f.DefaultVariation)
	return f, nil
}

func normalizeCondition(rc *rawConditionV2, path string) (Condition, error) {
	if rc.Attribute == "" {
		return Condition{}, cfgError(ReasonInvalidField, path+".attribute", "attribute is required")
	}
	op := Operator(rc.Operator)
	if op == "" {
		op = OpIn // operator 缺省视为 in
	}
	switch op {
	case OpIn, OpNotIn, OpStartsWith, OpGreaterThan, OpLessThan, OpEqual:
	default:
		return Condition{}, cfgError(ReasonInvalidField, path+".operator", "unknown operator: "+rc.Operator)
	}
	if len(rc.Values) == 0 {
		return Condition{}, cfgError(ReasonInvalidField, path+".values", "values must not be empty")
	}
	return Condition{Attribute: rc.Attribute, Operator: op, Values: append([]string(nil), rc.Values...)}, nil
}

func normalizeBucket(rb *rawBucketV2, path string) (*BucketSpec, error) {
	if len(rb.Variations) == 0 {
		return nil, cfgError(ReasonInvalidField, path+".variations", "bucket variations must not be empty")
	}
	if len(rb.Weights) != len(rb.Variations) {
		return nil, cfgError(ReasonInvalidField, path+".weights",
			fmt.Sprintf("weights length %d != variations length %d", len(rb.Weights), len(rb.Variations)))
	}
	sum := 0
	for _, w := range rb.Weights {
		if w < 0 || w > bucketSpace {
			return nil, cfgError(ReasonInvalidField, path+".weights",
				"each weight must be in [0,10000]")
		}
		sum += w
	}
	if sum != bucketSpace {
		return nil, cfgError(ReasonInvalidField, path+".weights",
			fmt.Sprintf("weights must sum to 10000, got %d", sum))
	}
	return &BucketSpec{
		Variations: append([]string(nil), rb.Variations...),
		Weights:    append([]int(nil), rb.Weights...),
		Salt:       rb.Salt,
	}, nil
}

// ValidateConfig 对已归一化的配置做跨字段与依赖图校验，
// 并对每个开关内的规则排序、变体声明补全。
func ValidateConfig(cfg *Config) error {
	if cfg == nil {
		return cfgError(ReasonInvalidField, "", "nil config")
	}

	// 第一遍：单开关内部一致性 + 变体声明补全。
	for key, f := range cfg.Flags {
		p := "flags." + key
		if f.Key == "" {
			f.Key = key
		}
		if f.DefaultVariation == "" {
			return cfgError(ReasonInvalidField, p+".default_variation", "default variation is required")
		}
		f.Variations = appendIfMissing(f.Variations, f.DefaultVariation)
		allowed := make(map[string]bool, len(f.Variations))
		for _, v := range f.Variations {
			if v == "" {
				return cfgError(ReasonInvalidField, p+".variations", "variation must not be empty")
			}
			allowed[v] = true
		}
		for i := range f.Rules {
			r := &f.Rules[i]
			if r.Bucket != nil {
				for _, v := range r.Bucket.Variations {
					if !allowed[v] {
						return cfgError(ReasonInvalidField,
							fmt.Sprintf("%s.rules[%d].bucket", p, i),
							"bucket variation "+v+" is not declared in flag variations")
					}
				}
			} else if !allowed[r.Variation] {
				return cfgError(ReasonInvalidField,
					fmt.Sprintf("%s.rules[%d].variation", p, i),
					"variation "+r.Variation+" is not declared in flag variations")
			}
		}
		sortRules(f)
	}

	// 第二遍：依赖图——缺失依赖、自依赖、环。
	if err := validatePrerequisites(cfg); err != nil {
		return err
	}

	// 第三遍：矛盾检测——前置要求的变体是被依赖开关永远不会产生的值。
	for key, f := range cfg.Flags {
		for i, pre := range f.Prerequisites {
			dep := cfg.Flags[pre.Flag] // 存在性已在第二遍确认
			produced := make(map[string]bool)
			produced[dep.DefaultVariation] = true
			for j := range dep.Rules {
				r := &dep.Rules[j]
				if r.Bucket != nil {
					for _, v := range r.Bucket.Variations {
						produced[v] = true
					}
				} else {
					produced[r.Variation] = true
				}
			}
			if !produced[pre.Variation] {
				return cfgError(ReasonContradiction,
					fmt.Sprintf("flags.%s.prerequisites[%d]", key, i),
					fmt.Sprintf("flag %q can never produce variation %q", pre.Flag, pre.Variation))
			}
		}
	}
	return nil
}

// validatePrerequisites 以 DFS 着色法检测依赖问题。
// 检查顺序固定：先缺失依赖 / 自依赖，再环；保证拒绝原因稳定可区分。
func validatePrerequisites(cfg *Config) error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(cfg.Flags))

	var visit func(key string, chain []string) error
	visit = func(key string, chain []string) error {
		color[key] = gray
		f := cfg.Flags[key]
		for i, pre := range f.Prerequisites {
			path := fmt.Sprintf("flags.%s.prerequisites[%d]", key, i)
			if pre.Flag == key {
				return cfgError(ReasonCycleDependency, path, "flag must not depend on itself")
			}
			if _, ok := cfg.Flags[pre.Flag]; !ok {
				return cfgError(ReasonMissingDependency, path,
					"prerequisite flag not defined: "+pre.Flag)
			}
			switch color[pre.Flag] {
			case gray:
				return cfgError(ReasonCycleDependency, path,
					fmt.Sprintf("dependency cycle: %s -> %s", joinChain(chain, key), pre.Flag))
			case white:
				if err := visit(pre.Flag, append(chain, key)); err != nil {
					return err
				}
			}
		}
		color[key] = black
		return nil
	}

	// 固定遍历顺序，保证同一份坏配置的报错信息稳定。
	keys := make([]string, 0, len(cfg.Flags))
	for k := range cfg.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if color[k] == white {
			if err := visit(k, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendIfMissing(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

func joinChain(chain []string, last string) string {
	out := ""
	for _, c := range chain {
		out += c + " -> "
	}
	return out + last
}

func wrapPath(prefix string, err error) error {
	ce, ok := err.(*ConfigError)
	if !ok {
		return err
	}
	if ce.Path == "" {
		ce.Path = prefix
	} else {
		ce.Path = prefix + "." + ce.Path
	}
	return ce
}

// ConfigSource 是后台刷新使用的配置数据源抽象（本地文件 / 远端下发均可实现）。
type ConfigSource interface {
	// Load 返回当前最新的原始配置与其来源标识。
	Load(ctx context.Context) ([]byte, string, error)
}

// ParseBytes 是 ParseConfig 的别名式入口，供刷新链路与测试使用，保证所有入口
// 走同一套解析/校验逻辑。
func ParseBytes(data []byte) (*Config, error) {
	return ParseConfig(data, "")
}
