//go:build linux

package proc

import (
	"os"
	"strconv"
)

func descendantPIDs(root int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	parents := make(map[int][]int)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, ok := readProcStat(pid)
		if !ok {
			continue
		}
		parents[stat.ppid] = append(parents[stat.ppid], pid)
	}
	return collectDescendants(root, parents)
}
