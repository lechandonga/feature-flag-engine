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
func RolloutValue(in RolloutInput) int {
	h := sha256.New()
	// Fixed, delimited encoding to avoid ambiguous concatenation.
	h.Write([]byte("ffe/v2\n"))
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
