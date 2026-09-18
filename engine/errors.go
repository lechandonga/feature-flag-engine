package engine

// 拒绝/降级原因。所有入口（初次加载、后台刷新、评估、上报）都只使用这一组
// 机器可识别的原因码，保证调用方可以区分不同的失败类型。
const (
	// ReasonUnsupportedVersion 配置的 schema 版本高于本引擎支持的最高版本。
	ReasonUnsupportedVersion = "unsupported_version"
	// ReasonInvalidField 字段取值非法，或必填字段缺失。
	ReasonInvalidField = "invalid_field"
	// ReasonMissingDependency 前置依赖引用了不存在的开关。
	ReasonMissingDependency = "missing_dependency"
	// ReasonCycleDependency 前置依赖构成环（含开关依赖自身）。
	ReasonCycleDependency = "cycle_dependency"
	// ReasonContradiction 配置自相矛盾（如依赖了目标开关永远不会产生的变体）。
	ReasonContradiction = "contradiction"
	// ReasonDuplicateDefinition 同一作用域内出现重复定义（重复 flag key / rule id）。
	ReasonDuplicateDefinition = "duplicate_definition"
	// ReasonConfigUnavailable 当前没有任何可用配置（初次加载失败且无历史快照）。
	ReasonConfigUnavailable = "config_unavailable"
	// ReasonConfigStale 最近一次刷新失败，当前返回的是上一份有效配置。
	ReasonConfigStale = "config_stale"
)

// ConfigError 描述一次配置被拒绝的具体原因。
// Reason 取上面的 Reason* 常量；Path 为出错字段的 JSON 风格路径。
type ConfigError struct {
	Reason string
	Path   string
	Msg    string
}

func (e *ConfigError) Error() string {
	if e.Path == "" {
		return e.Reason + ": " + e.Msg
	}
	return e.Reason + " at " + e.Path + ": " + e.Msg
}

func cfgError(reason, path, msg string) *ConfigError {
	return &ConfigError{Reason: reason, Path: path, Msg: msg}
}
