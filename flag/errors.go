package flag

import "errors"

// Sentinel errors returned by configuration parsing / validation.
// They are kept distinct so callers can distinguish rejection reasons.
var (
	// ErrInvalidConfig is the root wrapper for all configuration rejections.
	ErrInvalidConfig = errors.New("invalid flag configuration")

	ErrEmptyConfig      = errors.New("configuration is empty")
	ErrUnknownVersion   = errors.New("unknown or unsupported config version")
	ErrMalformedJSON    = errors.New("configuration payload is not valid JSON")
	ErrIllegalValue     = errors.New("illegal field value")
	ErrMissingField     = errors.New("required field is missing")
	ErrDuplicateFlagKey = errors.New("duplicate flag key")
	ErrDuplicateRuleID  = errors.New("duplicate rule id")
	ErrDuplicateVariant = errors.New("duplicate variant key")
	ErrWeightSum        = errors.New("bucket weights must sum to 10000 (basis points)")
	ErrEmptyVariants    = errors.New("rolled-out flag must define at least one variant")
	ErrBadPercentage    = errors.New("rollout percentage must be within [0,100]")
	ErrBadPriority      = errors.New("rule priority must be a positive integer")
	ErrUnknownOperator  = errors.New("unknown targeting operator")
	ErrBadCondition     = errors.New("malformed targeting condition")

	// Dependency-related rejections.
	ErrMissingDependency       = errors.New("flag depends on a non-existent flag")
	ErrCircularDependency      = errors.New("circular dependency detected")
	ErrDependencyContradiction = errors.New("flag contradicts its prerequisite configuration")
)
