//go:build darwin

package proc

import "golang.org/x/sys/unix"

func descendantPIDs(root int) []int {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	parents := make(map[int][]int)
	for _, process := range processes {
		parents[int(process.Eproc.Ppid)] = append(parents[int(process.Eproc.Ppid)], int(process.Proc.P_pid))
	}
	return collectDescendants(root, parents)
}
