//go:build darwin

package proc

import "golang.org/x/sys/unix"

func descendantPIDs(root int) []int {
	pids, _ := inventoryDescendants(root, false)
	return pids
}

func quiescencePIDs(root int) ([]int, error) { return inventoryDescendants(root, true) }

func inventoryDescendants(root int, includeOwnedGroup bool) ([]int, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	parents := make(map[int][]int)
	for _, process := range processes {
		parents[int(process.Eproc.Ppid)] = append(parents[int(process.Eproc.Ppid)], int(process.Proc.P_pid))
		if includeOwnedGroup && int(process.Eproc.Pgid) == root && int(process.Proc.P_pid) != root {
			parents[root] = append(parents[root], int(process.Proc.P_pid))
		}
	}
	return collectDescendants(root, parents), nil
}
