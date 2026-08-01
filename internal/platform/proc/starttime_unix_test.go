//go:build unix

package proc

import (
	"os/exec"
	"testing"
)

// TestStartTimeExitedProcessFails spawns and fully reaps a process, then
// checks its start time can no longer be read. As with Alive's own exited-
// process test, there's an inherent PID-reuse race (the window between
// reaping and the probe), accepted as tiny.
func TestStartTimeExitedProcessFails(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if _, ok := StartTime(pid); ok {
		t.Errorf("StartTime(%d) ok = true for a reaped process, want false", pid)
	}
}
