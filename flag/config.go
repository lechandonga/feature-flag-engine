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
//     a rule that explicitly writes "percentage": 0 means "match but never
//     serve" — the two spellings are intentionally different and every config
//     ingress (file, push, refresh, direct Install) preserves that difference;
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
		migrateV1(&raw)
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
func migrateV1(raw *RawConfig) {
	for i := range raw.Flags {
		f := &raw.Flags[i]
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
		// evaluation semantics stay rule-based.
		//
		// Backwards-compatibility note: in v1 there was no rule-level
		// "percentage" field at all, and the old engine treated the zero value
		// of flag-level "percent" as the omitted default (full rollout). To
		// keep old configs byte-for-byte behaviourally identical, 0 is migrated
		// as an *omitted* percentage (nil -> 100), never as an explicit zero.
		if f.Percent != nil && len(f.Rules) == 0 && f.On {
			rule := RawRule{
				ID:       f.Key + "-legacy-default",
				Priority: 1,
			}
			if *f.Percent != 0 {
				p := float64(*f.Percent)
				rule.Percentage = &p
			}
			f.Rules = []RawRule{rule}
		}
	}
	raw.Version = Version2
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
			// Explicit 0 is a legal, meaningful value ("match but never
			// serve"); only an omitted field receives the 100 default.
			pct := 100.0
			explicit := rr.Percentage != nil
			if rr.Percentage != nil {
				pct = *rr.Percentage
			}
			if pct < 0 || pct > 100 {
				return nil, fmt.Errorf("%w: %w: rule %q percentage=%v", ErrInvalidConfig, ErrBadPercentage, rr.ID, pct)
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
				Percentage: pct, PercentageExplicit: explicit,
				Variants: variants, Off: rr.Off,
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
// It is the same boundary ParseConfig enforces while normalizing raw JSON,
// restated here as an invariant check on an already-built Config so that the
// direct-install ingress (Engine.Install) cannot bypass it: configs built by
// hand still have to satisfy every structural rule. Dependency-graph checks
// (missing / self / cyclic / contradictory prerequisites) live in
// CheckDependencies and are run separately by both ingresses.
func (c *Config) Validate() error {
	if c == nil || len(c.Flags) == 0 {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, ErrEmptyConfig)
	}
	switch c.Version {
	case Version1, Version2:
	default:
		return fmt.Errorf("%w: %w: %q", ErrInvalidConfig, ErrUnknownVersion, c.Version)
	}
	for key, f := range c.Flags {
		if f == nil {
			return fmt.Errorf("%w: %w: nil flag entry", ErrInvalidConfig, ErrIllegalValue)
		}
		if f.Key == "" || f.Key != key {
			return fmt.Errorf("%w: %w: flag key %q / map key %q mismatch or empty",
				ErrInvalidConfig, ErrMissingField, f.Key, key)
		}
		seenRules := map[string]struct{}{}
		for _, r := range f.Rules {
			if r == nil {
				return fmt.Errorf("%w: %w: nil rule on flag %q", ErrInvalidConfig, ErrIllegalValue, key)
			}
			if r.ID == "" {
				return fmt.Errorf("%w: %w: rule id is required on flag %q", ErrInvalidConfig, ErrMissingField, key)
			}
			if _, dup := seenRules[r.ID]; dup {
				return fmt.Errorf("%w: %w: %q on flag %q", ErrInvalidConfig, ErrDuplicateRuleID, r.ID, key)
			}
			seenRules[r.ID] = struct{}{}
			if r.Priority <= 0 {
				return fmt.Errorf("%w: %w: rule %q priority=%d", ErrInvalidConfig, ErrBadPriority, r.ID, r.Priority)
			}
			if r.Percentage < 0 || r.Percentage > 100 {
				return fmt.Errorf("%w: %w: rule %q percentage=%v", ErrInvalidConfig, ErrBadPercentage, r.ID, r.Percentage)
			}
			for _, cond := range r.Conditions {
				if err := validateCondition(key, r.ID, cond); err != nil {
					return err
				}
			}
			if !r.Off {
				if err := validateVariants(key, r.ID, r.Variants); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateCondition(flagKey, ruleID string, cond Condition) error {
	if cond.Attribute == "" {
		return fmt.Errorf("%w: %w: empty attribute in rule %q of flag %q", ErrInvalidConfig, ErrBadCondition, ruleID, flagKey)
	}
	switch cond.Operator {
	case OpIn, OpNotIn, OpEqual, OpNotEqual, OpContains, OpStartsWith:
	default:
		return fmt.Errorf("%w: %w: %q in rule %q of flag %q", ErrInvalidConfig, ErrUnknownOperator, cond.Operator, ruleID, flagKey)
	}
	if (cond.Operator == OpIn || cond.Operator == OpNotIn) && len(cond.Values) == 0 {
		return fmt.Errorf("%w: %w: operator %q requires values in rule %q", ErrInvalidConfig, ErrBadCondition, cond.Operator, ruleID)
	}
	return nil
}

func validateVariants(flagKey, ruleID string, variants []Variant) error {
	if len(variants) == 0 {
		return fmt.Errorf("%w: %w: rule %q of flag %q", ErrInvalidConfig, ErrEmptyVariants, ruleID, flagKey)
	}
	keys := map[string]struct{}{}
	sum := 0
	for _, v := range variants {
		if v.Key == "" {
			return fmt.Errorf("%w: %w: variant key required in rule %q of flag %q", ErrInvalidConfig, ErrMissingField, ruleID, flagKey)
		}
		if _, dup := keys[v.Key]; dup {
			return fmt.Errorf("%w: %w: variant %q in rule %q", ErrInvalidConfig, ErrDuplicateVariant, v.Key, ruleID)
		}
		keys[v.Key] = struct{}{}
		if v.WeightBPS <= 0 {
			return fmt.Errorf("%w: %w: variant %q weightBPS=%d", ErrInvalidConfig, ErrIllegalValue, v.Key, v.WeightBPS)
		}
		sum += v.WeightBPS
	}
	if sum != rolloutBasis {
		return fmt.Errorf("%w: %w: rule %q weights sum to %d", ErrInvalidConfig, ErrWeightSum, ruleID, sum)
	}
	return nil
}
