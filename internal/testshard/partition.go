// Package testshard deterministically balances named tests across shards.
package testshard

import "sort"

// Partition assigns each distinct name to exactly one of pieces groups using
// longest-processing-time-first scheduling. It places heavier names first,
// breaking equal-weight ties by name, then chooses the lightest group, breaking
// equal-total ties by group index. Names within each returned group are sorted.
// The result depends on the set of names and their weights, not input order.
//
// pieces must be positive. weigh must return a stable, positive weight for
// every name, and the sum of distinct-name weights must fit in an int64.
// Partition does not modify names.
func Partition(names []string, pieces int, weigh func(string) int64) [][]string {
	unique := make(map[string]struct{}, len(names))
	for _, name := range names {
		unique[name] = struct{}{}
	}
	ordered := make([]string, 0, len(unique))
	for name := range unique {
		ordered = append(ordered, name)
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, right := weigh(ordered[i]), weigh(ordered[j])
		if left != right {
			return left > right
		}
		return ordered[i] < ordered[j]
	})
	groups := make([][]string, pieces)
	totals := make([]int64, pieces)
	for _, name := range ordered {
		target := 0
		for index := 1; index < len(groups); index++ {
			if totals[index] < totals[target] {
				target = index
			}
		}
		groups[target] = append(groups[target], name)
		totals[target] += weigh(name)
	}
	for _, group := range groups {
		sort.Strings(group)
	}
	return groups
}
