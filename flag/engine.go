package flag

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// Engine evaluates flags against an atomically-swapped configuration snapshot.
//
// The active config is held behind an atomic.Pointer, so a background refresh
// that installs a new snapshot can never expose partially-built state:
// concurrent Evaluate calls either see the complete previous config or the
// complete new one, with deterministic results in both cases.
type Engine struct {
	cfg atomic.Pointer[Config]
}

// NewEngine builds an engine with no configuration loaded.
func NewEngine() *Engine { return &Engine{} }

// Snapshot atomically installs a validated configuration. Passing nil is a
// no-op so a refresh failure can never clear the last good config.
func (e *Engine) Snapshot(c *Config) {
	if c == nil {
		return
	}
	e.cfg.Store(c)
}

// Current returns the active configuration version, "" when none loaded.
func (e *Engine) Current() Version {
	c := e.cfg.Load()
	if c == nil {
		return ""
	}
	return c.Version
}

// Evaluate returns a stable, explainable decision for one user/flag pair.
//
// Decision order:
//  1. no config loaded            -> StatusUnavailable;
//  2. unknown flag key            -> StatusFlagNotFound;
//  3. prerequisites evaluated first (in declared order); any not ON
//     -> StatusPrereqNotMet, flag OFF;
//  4. flag off with no match      -> OFF/default;
//  5. rules sorted by priority (lower wins; ties broken by id for a total,
//     stable order); first rule whose conditions all match fires:
//     - rule off        -> OFF (SourceRuleOff);
//     - rollout miss    -> OFF/default;
//     - rollout hit     -> ON + deterministic variant (SourceRule).
func (e *Engine) Evaluate(ctx context.Context, user User, flagKey string) Result {
	c := e.cfg.Load()
	if c == nil {
		return Result{FlagKey: flagKey, UserID: user.ID,
			Status: StatusUnavailable, Source: SourceConfigUnavailable,
			Reason: "no configuration has been loaded yet"}

	}
	f, ok := c.Flags[flagKey]
	if !ok {
		return Result{FlagKey: flagKey, UserID: user.ID,
			Status: StatusFlagNotFound, Source: SourceDefault,
			ConfigVersion: c.Version,
			Reason:        "flag is not defined in configuration " + string(c.Version)}
	}

	base := Result{FlagKey: flagKey, UserID: user.ID, ConfigVersion: c.Version}

	// Prerequisites gate every rule of this flag.
	for _, pre := range f.Prerequisites {
		if err := ctx.Err(); err != nil {
			r := base
			r.Status = StatusUnavailable
			r.Source = SourceConfigUnavailable
			r.Reason = "evaluation cancelled: " + err.Error()
			return r
		}
		pr := e.Evaluate(ctx, user, pre)
		if !pr.On {
			r := base
			r.Status = StatusPrereqNotMet
			r.Source = SourceDefault
			r.Reason = "prerequisite flag " + pre + " is not ON (status=" + string(pr.Status) + ")"
			return r
		}
	}

	r := base
	if !f.On {
		r.Status = StatusOff
		r.Source = SourceDefault
		r.Reason = "flag switch is off"
		return r
	}

	matched := matchRule(f, user)
	if matched == nil {
		r.Status = StatusOff
		r.Source = SourceDefault
		r.Reason = "no targeting rule matched the user attributes"
		return r
	}

	if matched.Off {
		r.Status = StatusOff
		r.Source = SourceRuleOff
		r.MatchedRule = matched.ID
		r.Reason = "rule " + matched.ID + " matched and forces the flag off"
		return r
	}

	bucket := RolloutValue(RolloutInput{FlagKey: flagKey, UserID: user.ID})
	cutoff := int(matched.Percentage*100 + 0.5) // percentage -> basis points
	if bucket >= cutoff {
		r.Status = StatusOff
		r.Source = SourceDefault
		r.MatchedRule = matched.ID
		r.Reason = "rule " + matched.ID + " matched but user bucket " +
			strconv.Itoa(bucket) + " is outside rollout window [0," + strconv.Itoa(cutoff) + ")"
		return r
	}

	variant := pickVariant(matched.Variants, bucket)
	r.Status = StatusOn
	r.On = true
	r.Variant = variant
	r.Source = SourceRule
	r.MatchedRule = matched.ID
	r.Reason = "rule " + matched.ID + " matched; bucket " + strconv.Itoa(bucket) +
		" within [0," + strconv.Itoa(cutoff) + ") selected variant " + variant
	return r
}

// matchRule returns the highest-priority rule whose conditions all match.
// Priority ties are broken by rule id so the order is total and independent
// of the order rules happened to be listed in (stability under rule reorder).
func matchRule(f *Flag, user User) *Rule {
	rules := make([]*Rule, len(f.Rules))
	copy(rules, f.Rules)
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority < rules[j].Priority
		}
		return rules[i].ID < rules[j].ID
	})
	for _, r := range rules {
		if ruleMatches(r, user) {
			return r
		}
	}
	return nil
}

func ruleMatches(r *Rule, user User) bool {
	for _, cond := range r.Conditions {
		if !conditionMatches(cond, user.Attributes[cond.Attribute]) {
			return false
		}
	}
	return true
}

func conditionMatches(c Condition, actual string) bool {
	switch c.Operator {
	case OpEqual:
		return len(c.Values) > 0 && actual == c.Values[0]
	case OpNotEqual:
		return len(c.Values) > 0 && actual != c.Values[0]
	case OpIn:
		for _, v := range c.Values {
			if actual == v {
				return true
			}
		}
		return false
	case OpNotIn:
		for _, v := range c.Values {
			if actual == v {
				return false
			}
		}
		return true
	case OpContains:
		return len(c.Values) > 0 && strings.Contains(actual, c.Values[0])
	case OpStartsWith:
		return len(c.Values) > 0 && strings.HasPrefix(actual, c.Values[0])
	default:
		// Unknown operators are rejected at parse time; be conservative here.
		return false
	}
}
