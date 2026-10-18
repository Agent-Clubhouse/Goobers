//go:build linux

package proc

import (
	"os"
	"strconv"
)

func descendantPIDs(root int) []int {
	pids, _ := inventoryDescendants(root, false)
	return pids
}

func quiescencePIDs(root int) ([]int, error) { return inventoryDescendants(root, true) }

func inventoryDescendants(root int, includeOwnedGroup bool) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
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
		if includeOwnedGroup && stat.session == root && pid != root {
			parents[root] = append(parents[root], pid)
		}
	}
	return collectDescendants(root, parents), nil
}
