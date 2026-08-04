//go:build linux

package proc

import (
	"os"
	"strconv"
	"strings"
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
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		closeParen := strings.LastIndexByte(string(data), ')')
		if closeParen < 0 {
			continue
		}
		fields := strings.Fields(string(data)[closeParen+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err == nil {
			parents[ppid] = append(parents[ppid], pid)
		}
	}
	return collectDescendants(root, parents)
}
