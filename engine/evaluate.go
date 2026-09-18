package engine

import (
	"sort"
	"strconv"
)

// maxPrerequisiteDepth 限制前置依赖递归深度。配置阶段已经拒绝环，这里只是
// 防止异常数据导致运行期栈增长。
const maxPrerequisiteDepth = 64

// evaluator 持有一份不可变配置快照，为 Engine 提供无锁只读评估。
type evaluator struct {
	cfg *Config
}

func newEvaluator(cfg *Config) *evaluator {
	return &evaluator{cfg: cfg}
}

// evaluate 对单个开关求值。调用方保证 flag 存在于快照中。
//
// visiting 用于在前置依赖递归求值时防止配置图中的环导致无限递归
// （理论上配置阶段已拒绝环，这里是运行时双保险）。
func (e *evaluator) evaluate(flagKey string, user User, visiting map[string]bool) Result {
	f := e.cfg.Flags[flagKey]
	if f == nil {
		return Result{
			Status:        StatusFlagNotFound,
			FlagKey:       flagKey,
			ConfigVersion: e.cfg.Version,
		}
	}
	base := Result{FlagKey: flagKey, ConfigVersion: e.cfg.Version}

	// 前置依赖检查：任一前置开关的求值结果不等于要求变体，则不满足。
	if len(visiting) >= maxPrerequisiteDepth || visiting[flagKey] {
		// 运行期检测到环/过深：安全降级为默认变体，而不是 panic 或返回空。
		base.Status = StatusPrerequisiteNotMet
		base.Variation = f.DefaultVariation
		base.Source = SourcePrerequisiteDefault
		base.Reason = ReasonCycleDependency
		return base
	}
	for _, pre := range f.Prerequisites {
		preVisiting := cloneVisiting(visiting)
		preVisiting[flagKey] = true
		got := e.evaluate(pre.Flag, user, preVisiting)
		if got.Status != StatusOK || got.Variation != pre.Variation {
			base.Status = StatusPrerequisiteNotMet
			base.Variation = f.DefaultVariation
			base.Source = SourcePrerequisiteDefault
			return base
		}
	}

	// 规则按优先级匹配，首条命中生效。
	for i := range f.Rules {
		r := &f.Rules[i]
		if !matchRule(r, user) {
			continue
		}
		base.Status = StatusOK
		base.HitRuleID = r.ID
		if r.Bucket != nil {
			key := bucketKey(r.Bucket.Salt, flagKey, r.ID, user.ID)
			slot := hashToSlot(key)
			base.Variation = pickVariation(r.Bucket, slot)
			base.Source = SourceBucket
			base.BucketKey = key
		} else {
			base.Variation = r.Variation
			base.Source = SourceRule
		}
		return base
	}

	// 无规则命中：取默认变体。
	base.Status = StatusOK
	base.Variation = f.DefaultVariation
	base.Source = SourceDefault
	return base
}

func cloneVisiting(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m)+1)
	for k := range m {
		out[k] = true
	}
	return out
}

// matchRule 判断用户是否命中规则的全部条件（AND 语义；无条件的规则命中所有人）。
func matchRule(r *Rule, user User) bool {
	for _, c := range r.Match {
		if !matchCondition(c, user) {
			return false
		}
	}
	return true
}

// matchCondition 判断单个条件是否成立。
// 属性缺失时：仅 not_in 视为成立（“不在白名单”），其余操作符一律不成立。
func matchCondition(c Condition, user User) bool {
	v, ok := user.Attr(c.Attribute)
	switch c.Operator {
	case OpIn:
		if !ok {
			return false
		}
		return contains(c.Values, v)
	case OpNotIn:
		if !ok {
			return true
		}
		return !contains(c.Values, v)
	case OpStartsWith:
		if !ok || len(c.Values) == 0 {
			return false
		}
		return hasPrefix(v, c.Values)
	case OpEqual:
		if !ok || len(c.Values) == 0 {
			return false
		}
		return v == c.Values[0]
	case OpGreaterThan:
		if !ok || len(c.Values) == 0 {
			return false
		}
		x, errx := strconv.ParseFloat(v, 64)
		y, erry := strconv.ParseFloat(c.Values[0], 64)
		if errx != nil || erry != nil {
			return false
		}
		return x > y
	case OpLessThan:
		if !ok || len(c.Values) == 0 {
			return false
		}
		x, errx := strconv.ParseFloat(v, 64)
		y, erry := strconv.ParseFloat(c.Values[0], 64)
		if errx != nil || erry != nil {
			return false
		}
		return x < y
	default:
		// 未知操作符在校验阶段已被拒绝，运行期视为不匹配，保证安全失败。
		return false
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func hasPrefix(v string, prefixes []string) bool {
	for _, p := range prefixes {
		if len(v) >= len(p) && v[:len(p)] == p {
			return true
		}
	}
	return false
}

// sortRules 按 (Priority 升序, ID 升序) 原地排序规则，
// 保证匹配顺序只取决于规则自身属性，与配置中的书写顺序无关。
func sortRules(f *Flag) {
	sort.SliceStable(f.Rules, func(i, j int) bool {
		if f.Rules[i].Priority != f.Rules[j].Priority {
			return f.Rules[i].Priority < f.Rules[j].Priority
		}
		return f.Rules[i].ID < f.Rules[j].ID
	})
}
