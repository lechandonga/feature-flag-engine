package engine

import (
	"testing"
)

func TestHashToSlotStableAcrossCalls(t *testing.T) {
	key := bucketKey("dark-mode", "dark-mode", "rollout", "user-123")
	first := hashToSlot(key)
	for i := 0; i < 100; i++ {
		if got := hashToSlot(key); got != first {
			t.Fatalf("slot not stable: %d vs %d", got, first)
		}
	}
}

func TestBucketKeyDeterminism(t *testing.T) {
	// 相同输入任意评估顺序产生相同 key。
	k1 := bucketKey("s", "flag-a", "rule-1", "u1")
	k2 := bucketKey("s", "flag-a", "rule-1", "u1")
	if k1 != k2 {
		t.Fatalf("bucket key mismatch: %q vs %q", k1, k2)
	}
	// 不同用户 / 规则 / 开关必须隔离。
	cases := [][2]string{
		{bucketKey("s", "flag-a", "rule-1", "u2"), k1},
		{bucketKey("s", "flag-a", "rule-2", "u1"), k1},
		{bucketKey("s", "flag-b", "rule-1", "u1"), k1},
	}
	for _, c := range cases {
		if c[0] == c[1] {
			t.Fatalf("expected different bucket keys, both %q", c[0])
		}
	}
}

func TestPickVariationWeights(t *testing.T) {
	spec := &BucketSpec{
		Variations: []string{"off", "a", "b"},
		Weights:    []int{5000, 2500, 2500},
	}
	cases := map[uint32]string{
		0:    "off",
		4999: "off",
		5000: "a",
		7499: "a",
		7500: "b",
		9999: "b",
	}
	for slot, want := range cases {
		if got := pickVariation(spec, slot); got != want {
			t.Fatalf("slot %d => %s, want %s", slot, got, want)
		}
	}
}

func TestBucketDistributionRoughlyMatchesWeights(t *testing.T) {
	spec := &BucketSpec{
		Variations: []string{"control", "treatment"},
		Weights:    []int{5000, 5000},
	}
	counts := map[string]int{}
	for i := 0; i < 20000; i++ {
		key := bucketKey("exp1", "exp1", "r1", userSeq(i))
		v := pickVariation(spec, hashToSlot(key))
		counts[v]++
	}
	// 50/50 分桶，允许 3% 偏差。
	for _, want := range []string{"control", "treatment"} {
		ratio := float64(counts[want]) / 20000.0
		if ratio < 0.47 || ratio > 0.53 {
			t.Fatalf("variation %s ratio %.3f out of range", want, ratio)
		}
	}
}

func userSeq(i int) string {
	return "user-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
