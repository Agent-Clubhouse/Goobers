//go:build unix

package proc

import (
	"strconv"
	"strings"
)

func collectDescendants(root int, parents map[int][]int) []int {
	var descendants []int
	queue := append([]int(nil), parents[root]...)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		descendants = append(descendants, pid)
		queue = append(queue, parents[pid]...)
	}
	return descendants
}

func descendantsFromPS(root int, output []byte) []int {
	fields := strings.Fields(string(output))
	parents := make(map[int][]int)
	for i := 0; i+1 < len(fields); i += 2 {
		pid, pidErr := strconv.Atoi(fields[i])
		ppid, ppidErr := strconv.Atoi(fields[i+1])
		if pidErr == nil && ppidErr == nil {
			parents[ppid] = append(parents[ppid], pid)
		}
	}
	return collectDescendants(root, parents)
}
