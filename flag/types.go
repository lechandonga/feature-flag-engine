package flag

// Version is the schema version of a configuration payload.
type Version string

const (
	// Version1 is the legacy schema (2023): rollout as percent integer,
	// weight expressed in percentage units, rule "order" instead of "priority".
	Version1 Version = "v1"
	// Version2 is the current schema: rollout basis points, explicit
	// priority, condition operators, prerequisite flags.
	Version2 Version = "v2"
)

// RawConfig is the on-the-wire configuration envelope.
//
// Forward-compatibility policy: unknown fields are silently ignored at every
// ingress point (files, API payloads, refresh payloads) so that a server
// newer than this client never breaks evaluation. Unknown versions and
// illegal known values are rejected with a typed error instead.
type RawConfig struct {
	Version Version   `json:"version"`
	Flags   []RawFlag `json:"flags"`
}

// RawFlag is one switch definition as received on the wire.
type RawFlag struct {
	Key           string    `json:"key"`
	Description   string    `json:"description,omitempty"`
	On            bool      `json:"on"`
	Rules         []RawRule `json:"rules"`
	Prerequisites []string  `json:"prerequisites,omitempty"`
	// V1-only legacy fields (also tolerated on v2 as migration input).
	Percent *int `json:"percent,omitempty"`
}

// RawRule is a targeting rule as received on the wire.
type RawRule struct {
	ID string `json:"id"`
	// Priority is 1-based; lower value wins. V1 payloads use "order".
	Priority   int            `json:"priority"`
	Conditions []RawCondition `json:"conditions,omitempty"`
	// Percentage within 0..100; rolled-out users are then bucketed.
	Percentage float64      `json:"percentage"`
	Variants   []RawVariant `json:"variants"`
	// Off means a matched rule forces the flag OFF regardless of percentage.
	Off bool `json:"off,omitempty"`

	// V1-only legacy aliases.
	Order  *int          `json:"order,omitempty"`
	Weight []RawV1Weight `json:"weight,omitempty"`
}

// RawV1Weight is the legacy per-variant weight in percentage units.
type RawV1Weight struct {
	Variant string `json:"variant"`
	Percent int    `json:"percent"`
}

// RawCondition is one attribute predicate. All predicates in a rule
// must match (AND semantics).
type RawCondition struct {
	Attribute string      `json:"attribute"`
	Operator  string      `json:"operator"`
	Values    []string    `json:"values,omitempty"`
	Value     interface{} `json:"value,omitempty"`
}

// RawVariant is a bucket destination.
type RawVariant struct {
	Key string `json:"key"`
	// WeightBPS is the bucket weight in basis points (1/10000); must sum to 10000.
	WeightBPS int `json:"weightBPS"`
}

// Condition is the validated, normalized predicate.
type Condition struct {
	Attribute string
	Operator  Operator
	Values    []string
}

// Operator enumerates supported targeting operators.
type Operator string

const (
	OpIn         Operator = "in"
	OpNotIn      Operator = "not_in"
	OpEqual      Operator = "eq"
	OpNotEqual   Operator = "ne"
	OpContains   Operator = "contains"
	OpStartsWith Operator = "starts_with"
)

// Variant is a validated bucket destination.
type Variant struct {
	Key       string
	WeightBPS int
}

// Rule is a validated targeting rule.
type Rule struct {
	ID         string
	Priority   int
	Conditions []Condition
	Percentage float64 // 0..100
	Variants   []Variant
	Off        bool
}

// Flag is a validated switch definition.
type Flag struct {
	Key           string
	Description   string
	On            bool
	Rules         []*Rule
	Prerequisites []string
}

// Config is an immutable, validated configuration snapshot.
type Config struct {
	Version Version
	Flags   map[string]*Flag
}

// User is the evaluation subject.
type User struct {
	ID         string
	Attributes map[string]string
}

// Source explains where an evaluated value came from.
type Source string

const (
	// SourceDefault: flag is OFF or no rule matched. Value is the static default.
	SourceDefault Source = "default"
	// SourceRule: a targeting rule matched and the user fell in its rollout.
	SourceRule Source = "rule"
	// SourceRuleOff: a rule matched and forced the flag off.
	SourceRuleOff Source = "rule_off"
	// SourceConfigUnavailable: no valid configuration has ever loaded.
	SourceConfigUnavailable Source = "config_unavailable"
)

// Status is the coarse evaluation outcome.
type Status string

const (
	StatusOn           Status = "on"
	StatusOff          Status = "off"
	StatusFlagNotFound Status = "flag_not_found"
	StatusUnavailable  Status = "unavailable"
	StatusPrereqNotMet Status = "prereq_not_met"
)

// Result is the explainable evaluation outcome.
type Result struct {
	FlagKey string
	UserID  string
	Status  Status
	On      bool
	Variant string
	Source  Source
	// MatchedRule is the rule id that fired, empty when Source is default/unavailable.
	MatchedRule string
	// Reason is a human-readable explanation of the decision path.
	Reason        string
	ConfigVersion Version
}
