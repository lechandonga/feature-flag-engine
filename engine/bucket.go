package engine

import (
	"hash/fnv"
)

// 分桶空间：0..9999，共 10000 个槽位，权重以万分比表示（100% = 10000）。
const bucketSpace = 10000

// bucketKey 构造分桶哈希的输入。
// 组成固定为 salt + flagKey + ruleID + userID：
//   - 含 flagKey（默认 salt 即 flagKey）：不同开关互不影响；
//   - 含 ruleID：规则重排不会改变某条命中规则对应的分桶；
//   - 含 userID：同一用户任意评估顺序、进程重启后结果一致。
//
// 该函数刻意不包含权重、变体顺序等内容：这些信息变动只影响槽位->变体的映射，
// 而用户所在槽位保持稳定。
func bucketKey(salt, flagKey, ruleID, userID string) string {
	if salt == "" {
		salt = flagKey
	}
	return salt + "|" + flagKey + "|" + ruleID + "|" + userID
}

// hashToSlot 将分桶键稳定映射到 [0, bucketSpace)。
// 使用 FNV-1a 64 位：纯函数、无随机种子，跨进程结果一致。
func hashToSlot(key string) uint32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return uint32(h.Sum64() % uint64(bucketSpace))
}

// pickVariation 按权重槽位选择变体；spec 已在校验阶段保证合法。
// 槽位边界按变体声明顺序累加，因此“在尾部新增变体 / 调大尾部权重”不会改变
// 既有用户在未变动槽位区间内的归属。
func pickVariation(spec *BucketSpec, slot uint32) string {
	if spec == nil || len(spec.Variations) == 0 {
		return ""
	}
	cum := 0
	for i, w := range spec.Weights {
		cum += w
		if int(slot) < cum {
			return spec.Variations[i]
		}
	}
	// 理论不可达（权重总和 == bucketSpace），兜底返回最后一个变体。
	return spec.Variations[len(spec.Variations)-1]
}
