//go:build unix && !linux && !darwin

package proc

import (
	"os/exec"
	"strconv"
	"strings"
)

func descendantPIDs(root int) []int {
	output, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=").Output()
	if err != nil {
		return nil
	}
	return descendantsFromPS(root, output)
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
