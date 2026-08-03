//go:build unix && !linux && !darwin

package proc

import "os/exec"

func descendantPIDs(root int) []int {
	output, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=").Output()
	if err != nil {
		return nil
	}
	return descendantsFromPS(root, output)
}
