package flag

import (
	"crypto/sha256"
	"encoding/binary"
)

// rolloutBasis is the resolution of the bucketing space in basis points.
// 10000 buckets => 0.01% granularity.
const rolloutBasis = 10000

// RolloutInput is the stable hashing input.
type RolloutInput struct {
	FlagKey string
	UserID  string
	Salt    string
}

// RolloutValue returns a deterministic integer in [0,10000) derived only
// from the flag key, user id and an optional stable salt. It deliberately
// excludes rule ids, rule order and timestamps so that reordering rules or
// restarting the process cannot move a user between buckets.
//
// The same (flagKey,userID,salt) always maps to the same bucket, even across
// processes or machines, because the hash inputs are pure data.
//
// RolloutValue answers ONLY the eligibility question ("is this user inside
// the rollout window?"). Variant assignment uses the independent
// VariantValue hash below, so that every declared variant — not just the
// first one on the cumulative-weight curve — is reachable by users who pass
// the eligibility gate.
//
// The hash encoding/domain MUST stay byte-identical forever: changing it
// would reshuffle every user's eligibility and break the monotonic-rollout
// guarantee for already-shipped configs.
func RolloutValue(in RolloutInput) int {
	return stableBucket("ffe/v2\n", in)
}

// VariantValue returns a deterministic integer in [0,10000) used ONLY for
// variant assignment. It is intentionally independent from RolloutValue:
//
//   - distinct domain prefix, so the two hashes are uncorrelated — within a
//     partial rollout the variants distribute according to their declared
//     weights over ALL rolled-in users (all variants reachable);
//   - same pure-data inputs (flag key, user id, salt) and the same
//     exclusions (no rule id, order, timestamp, version, process state), so
//     a user's variant is stable across evaluation order, process restarts
//     and rule reordering, and stays glued to the user while the rollout
//     percentage is raised (raising the window only admits NEW users; it
//     never re-hashes anyone already admitted).
func VariantValue(in RolloutInput) int {
	return stableBucket("ffe/v2/variant\n", in)
}

func stableBucket(domain string, in RolloutInput) int {
	h := sha256.New()
	// Fixed, delimited encoding to avoid ambiguous concatenation.
	h.Write([]byte(domain))
	h.Write([]byte(in.FlagKey))
	h.Write([]byte{'\n'})
	h.Write([]byte(in.Salt))
	h.Write([]byte{'\n'})
	h.Write([]byte(in.UserID))
	sum := h.Sum(nil)
	v := binary.BigEndian.Uint64(sum[:8])
	return int(v % rolloutBasis)
}

// pickVariant walks the cumulative weight curve and selects the variant
// covering bucket value v. Weights are basis points that sum to 10000.
// v here is always a VariantValue bucket, never the RolloutValue bucket.
func pickVariant(variants []Variant, v int) string {
	cum := 0
	for _, vt := range variants {
		cum += vt.WeightBPS
		if v < cum {
			return vt.Key
		}
	}
	// Defensive fallback; validated configs never reach this because the
	// weights are required to sum exactly to rolloutBasis.
	return variants[len(variants)-1].Key
}
