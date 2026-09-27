package flag

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Default rollout behaviour documented explicitly:
//
//   - a flag with no rules is fully OFF when "on":false (the zero value),
//     and fully ON (100%, single implicit "on" variant) when "on":true and
//     it has zero rules;
//   - a rule with no conditions matches every user;
//   - a rule with "percentage" omitted defaults to 100 (roll out fully on match);
//     an explicit "percentage": 0 means the rule matches but serves nobody
//     (the result still reports the matched rule and the reason);
//   - a rule with one variant and no weights defaults that variant to 10000 bps;
//   - "priority" omitted is a rejection on v2 (silent ordering changes would be
//     dangerous); v1 "order" is migrated to priority automatically.

// ParseConfig decodes a JSON payload into a validated immutable Config.
//
// It is the single ingress for all configuration sources (initial file,
// API pushes, background refreshes), so unknown-field / illegal-value /
// default-value handling is identical everywhere:
//   - unknown JSON fields are ignored (forward compatibility);
//   - unknown schema versions are rejected (ErrUnknownVersion);
//   - illegal known values are rejected with a typed error;
//   - omitted optional fields receive documented defaults.
//
// v1 payloads are migrated transparently to v2 semantics.
func ParseConfig(data []byte) (*Config, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, ErrEmptyConfig)
	}

	var raw RawConfig
	// Unknown fields are intentionally tolerated (standard decoder behaviour)
	// so newer servers can push extra data to older clients without breaking
	// evaluation. Known fields with bad values are rejected during normalize.
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %w: %v", ErrInvalidConfig, ErrMalformedJSON, err)
	}

	switch raw.Version {
	case Version1:
		if err := migrateV1(&raw); err != nil {
			return nil, err
		}
	case Version2:
		// current schema
	case "":
		return nil, fmt.Errorf("%w: %w: version is required", ErrInvalidConfig, ErrUnknownVersion)
	default:
		return nil, fmt.Errorf("%w: %w: %q", ErrInvalidConfig, ErrUnknownVersion, raw.Version)
	}

	cfg, err := normalize(raw)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if rj := cfg.CheckDependencies(); rj != nil {
		return nil, fmt.Errorf("%w: %w: %s", ErrInvalidConfig, rj.Kind, rj.Detail)
	}
	return cfg, nil
}

// migrateV1 rewrites legacy v1 fields in-place into v2 semantics:
//
//   - rule "order" (1-based) becomes "priority";
//   - flag "percent" becomes an implicit single full-match rule;
//   - legacy "weight" entries (integer percentages summing to 100) become
//     basis-point variants (weightBPS = percent*100).
//
// Legacy validation is preserved: a flag-level percent outside [0,100] was
// illegal under v1 and stays illegal after upgrade — it is rejected here
// instead of being silently treated as the omitted (full-rollout) default.
func migrateV1(raw *RawConfig) error {
	for i := range raw.Flags {
		f := &raw.Flags[i]
		if f.Percent != nil && (*f.Percent < 0 || *f.Percent > 100) {
			return fmt.Errorf("%w: %w: flag %q percent=%d", ErrInvalidConfig, ErrBadPercentage, f.Key, *f.Percent)
		}
		for j := range f.Rules {
			r := &f.Rules[j]
			if r.Order != nil && r.Priority == 0 {
				r.Priority = *r.Order
			}
			if len(r.Weight) > 0 && len(r.Variants) == 0 {
				for _, w := range r.Weight {
					r.Variants = append(r.Variants, RawVariant{Key: w.Variant, WeightBPS: w.Percent * 100})
				}
				r.Weight = nil
			}
		}
		// Legacy flag-level percent: wrap into one catch-all rule so that
		// evaluation semantics stay rule-based. Legacy semantics treated
		// percent:0 exactly like an omitted value (full rollout), so a 0 is
		// migrated to an omitted percentage to keep upgraded v1 configs
		// behaving exactly as before.
		if f.Percent != nil && len(f.Rules) == 0 && f.On {
			rule := RawRule{ID: f.Key + "-legacy-default", Priority: 1}
			if *f.Percent > 0 {
				p := float64(*f.Percent)
				rule.Percentage = &p
			}
			f.Rules = []RawRule{rule}
		}
	}
	raw.Version = Version2
	return nil
}

func normalize(raw RawConfig) (*Config, error) {
	cfg := &Config{Version: raw.Version, Flags: map[string]*Flag{}}
	for _, rf := range raw.Flags {
		if rf.Key == "" {
			return nil, fmt.Errorf("%w: %w: flag key is required", ErrInvalidConfig, ErrMissingField)
		}
		if _, dup := cfg.Flags[rf.Key]; dup {
			return nil, fmt.Errorf("%w: %w: %q", ErrInvalidConfig, ErrDuplicateFlagKey, rf.Key)
		}
		f := &Flag{Key: rf.Key, Description: rf.Description, On: rf.On, Prerequisites: rf.Prerequisites}

		ruleIDs := map[string]struct{}{}
		for _, rr := range rf.Rules {
			if rr.ID == "" {
				return nil, fmt.Errorf("%w: %w: rule id is required on flag %q", ErrInvalidConfig, ErrMissingField, rf.Key)
			}
			if _, dup := ruleIDs[rr.ID]; dup {
				return nil, fmt.Errorf("%w: %w: %q on flag %q", ErrInvalidConfig, ErrDuplicateRuleID, rr.ID, rf.Key)
			}
			ruleIDs[rr.ID] = struct{}{}
			if rr.Priority <= 0 {
				return nil, fmt.Errorf("%w: %w: rule %q priority=%d", ErrInvalidConfig, ErrBadPriority, rr.ID, rr.Priority)
			}
			// Omitted percentage defaults to 100 (full rollout on match);
			// an explicit 0 is preserved and means "matched, serve nobody".
			pct := 100.0
			if rr.Percentage != nil {
				pct = *rr.Percentage
				if pct < 0 || pct > 100 {
					return nil, fmt.Errorf("%w: %w: rule %q percentage=%v", ErrInvalidConfig, ErrBadPercentage, rr.ID, pct)
				}
			}
			conds, err := normalizeConditions(rf.Key, rr)
			if err != nil {
				return nil, err
			}
			variants, err := normalizeVariants(rf.Key, rr)
			if err != nil {
				return nil, err
			}
			f.Rules = append(f.Rules, &Rule{
				ID: rr.ID, Priority: rr.Priority, Conditions: conds,
				Percentage: pct, Variants: variants, Off: rr.Off,
			})
		}

		// A switched-on flag without any rule gets an implicit 100% rule with
		// a single "on" variant, so callers always have a concrete value.
		if f.On && len(f.Rules) == 0 {
			f.Rules = []*Rule{{
				ID: "implicit-on", Priority: 1, Percentage: 100,
				Variants: []Variant{{Key: "on", WeightBPS: 10000}},
			}}
		}
		cfg.Flags[rf.Key] = f
	}
	return cfg, nil
}

func normalizeConditions(flagKey string, rr RawRule) ([]Condition, error) {
	out := make([]Condition, 0, len(rr.Conditions))
	for _, rc := range rr.Conditions {
		if rc.Attribute == "" {
			return nil, fmt.Errorf("%w: %w: empty attribute in rule %q of flag %q", ErrInvalidConfig, ErrBadCondition, rr.ID, flagKey)
		}
		op := Operator(rc.Operator)
		switch op {
		case OpIn, OpNotIn, OpEqual, OpNotEqual, OpContains, OpStartsWith:
		default:
			return nil, fmt.Errorf("%w: %w: %q in rule %q of flag %q", ErrInvalidConfig, ErrUnknownOperator, rc.Operator, rr.ID, flagKey)
		}
		vals := rc.Values
		if len(vals) == 0 && rc.Value != nil {
			vals = []string{fmt.Sprintf("%v", rc.Value)}
		}
		if (op == OpIn || op == OpNotIn) && len(vals) == 0 {
			return nil, fmt.Errorf("%w: %w: operator %q requires values in rule %q", ErrInvalidConfig, ErrBadCondition, op, rr.ID)
		}
		out = append(out, Condition{Attribute: rc.Attribute, Operator: op, Values: vals})
	}
	return out, nil
}

// validateConditions applies the same condition checks to an already-built
// Rule that normalizeConditions applies to the wire form: non-empty
// attribute, known operator, and at least one candidate value for the set
// operators. Without this, a hand-built condition with an unknown operator
// would silently match nobody (or, for not_in with no values, everybody),
// diverging from the parsing ingress which rejects both.
func validateConditions(flagKey string, r *Rule) error {
	for _, cond := range r.Conditions {
		if cond.Attribute == "" {
			return fmt.Errorf("%w: %w: empty attribute in rule %q of flag %q", ErrInvalidConfig, ErrBadCondition, r.ID, flagKey)
		}
		switch cond.Operator {
		case OpIn, OpNotIn, OpEqual, OpNotEqual, OpContains, OpStartsWith:
		default:
			return fmt.Errorf("%w: %w: %q in rule %q of flag %q", ErrInvalidConfig, ErrUnknownOperator, cond.Operator, r.ID, flagKey)
		}
		if (cond.Operator == OpIn || cond.Operator == OpNotIn) && len(cond.Values) == 0 {
			return fmt.Errorf("%w: %w: operator %q requires values in rule %q", ErrInvalidConfig, ErrBadCondition, cond.Operator, r.ID)
		}
	}
	return nil
}

func normalizeVariants(flagKey string, rr RawRule) ([]Variant, error) {
	if rr.Off {
		return nil, nil
	}
	if len(rr.Variants) == 0 {
		// Default: single implicit "on" variant at full weight.
		return []Variant{{Key: "on", WeightBPS: 10000}}, nil
	}
	out := make([]Variant, 0, len(rr.Variants))
	sum := 0
	keys := map[string]struct{}{}
	for _, rv := range rr.Variants {
		if rv.Key == "" {
			return nil, fmt.Errorf("%w: %w: variant key required in rule %q of flag %q", ErrInvalidConfig, ErrMissingField, rr.ID, flagKey)
		}
		if _, dup := keys[rv.Key]; dup {
			return nil, fmt.Errorf("%w: %w: variant %q in rule %q", ErrInvalidConfig, ErrDuplicateVariant, rv.Key, rr.ID)
		}
		keys[rv.Key] = struct{}{}
		if rv.WeightBPS <= 0 {
			return nil, fmt.Errorf("%w: %w: variant %q weightBPS=%d", ErrInvalidConfig, ErrIllegalValue, rv.Key, rv.WeightBPS)
		}
		sum += rv.WeightBPS
		out = append(out, Variant{Key: rv.Key, WeightBPS: rv.WeightBPS})
	}
	if sum != 10000 {
		return nil, fmt.Errorf("%w: %w: rule %q weights sum to %d", ErrInvalidConfig, ErrWeightSum, rr.ID, sum)
	}
	return out, nil
}

// Validate performs structural validation independent of dependency checks.
//
// It is applied both by ParseConfig and by Engine.Install, so a hand-built
// Config that bypasses parsing is held to exactly the same standard as one
// decoded from JSON: no empty config, no empty flag keys, the map key must
// match the flag's declared key, no duplicate rule ids, priorities > 0,
// percentages within [0,100], conditions with known operators and (for the
// set operators) at least one candidate value, and variant weights that are
// positive and sum to 10000 basis points.
func (c *Config) Validate() error {
	if c == nil || len(c.Flags) == 0 {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, ErrEmptyConfig)
	}
	for key, f := range c.Flags {
		if f == nil || key == "" || f.Key == "" {
			return fmt.Errorf("%w: %w: flag key is required", ErrInvalidConfig, ErrMissingField)
		}
		if f.Key != key {
			return fmt.Errorf("%w: %w: map key %q vs declared key %q", ErrInvalidConfig, ErrFlagKeyMismatch, key, f.Key)
		}
		ruleIDs := map[string]struct{}{}
		for _, r := range f.Rules {
			if r == nil || r.ID == "" {
				return fmt.Errorf("%w: %w: rule id is required on flag %q", ErrInvalidConfig, ErrMissingField, key)
			}
			if _, dup := ruleIDs[r.ID]; dup {
				return fmt.Errorf("%w: %w: %q on flag %q", ErrInvalidConfig, ErrDuplicateRuleID, r.ID, key)
			}
			ruleIDs[r.ID] = struct{}{}
			if r.Priority <= 0 {
				return fmt.Errorf("%w: %w: rule %q priority=%d", ErrInvalidConfig, ErrBadPriority, r.ID, r.Priority)
			}
			if r.Percentage < 0 || r.Percentage > 100 {
				return fmt.Errorf("%w: %w: rule %q percentage=%v", ErrInvalidConfig, ErrBadPercentage, r.ID, r.Percentage)
			}
			if err := validateConditions(key, r); err != nil {
				return err
			}
			if r.Off {
				continue
			}
			if len(r.Variants) == 0 {
				return fmt.Errorf("%w: %w: rule %q on flag %q", ErrInvalidConfig, ErrEmptyVariants, r.ID, key)
			}
			sum := 0
			keys := map[string]struct{}{}
			for _, v := range r.Variants {
				if v.Key == "" {
					return fmt.Errorf("%w: %w: variant key required in rule %q of flag %q", ErrInvalidConfig, ErrMissingField, r.ID, key)
				}
				if _, dup := keys[v.Key]; dup {
					return fmt.Errorf("%w: %w: variant %q in rule %q", ErrInvalidConfig, ErrDuplicateVariant, v.Key, r.ID)
				}
				keys[v.Key] = struct{}{}
				if v.WeightBPS <= 0 {
					return fmt.Errorf("%w: %w: variant %q weightBPS=%d", ErrInvalidConfig, ErrIllegalValue, v.Key, v.WeightBPS)
				}
				sum += v.WeightBPS
			}
			if sum != 10000 {
				return fmt.Errorf("%w: %w: rule %q weights sum to %d", ErrInvalidConfig, ErrWeightSum, r.ID, sum)
			}
		}
	}
	return nil
}
