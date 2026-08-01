package proc

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestStartTimeSelfIsPlausible doesn't assert an exact value (the whole point
// of StartTime is to compare it against a second call for the SAME pid later,
// not against a wall-clock expectation) — only that it's a real answer inside
// a generous window, tolerant of a long-running test session or clock skew.
func TestStartTimeSelfIsPlausible(t *testing.T) {
	start, ok := StartTime(os.Getpid())
	if !ok {
		t.Fatal("StartTime(self) ok = false, want true")
	}
	if since := time.Since(start); since < 0 || since > 24*time.Hour {
		t.Fatalf("StartTime(self) = %s, %s ago; want within the last 24h and not in the future", start, since)
	}
}

// TestStartTimeIsStableAcrossCalls is the property PID-reuse detection
// depends on: querying the SAME live pid twice must return the identical
// value, not something that drifts call to call (e.g. a mis-parsed relative
// duration).
func TestStartTimeIsStableAcrossCalls(t *testing.T) {
	first, ok := StartTime(os.Getpid())
	if !ok {
		t.Fatal("first StartTime(self) ok = false")
	}
	second, ok := StartTime(os.Getpid())
	if !ok {
		t.Fatal("second StartTime(self) ok = false")
	}
	if !first.Equal(second) {
		t.Fatalf("StartTime(self) = %s then %s, want identical values for the same live pid", first, second)
	}
}

func TestStartTimeNonPositivePIDFails(t *testing.T) {
	for _, pid := range []int{0, -1, -12345} {
		if _, ok := StartTime(pid); ok {
			t.Errorf("StartTime(%d) ok = true, want false", pid)
		}
	}
}

// TestStartTimeExitedProcessFails spawns and fully reaps a process, then
// checks its start time can no longer be read. As with Alive's own exited-
// process test, there's an inherent PID-reuse race (the window between
// reaping and the probe), accepted as tiny.
func TestStartTimeExitedProcessFails(t *testing.T) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", "exit 0")
	} else {
		cmd = exec.Command("sh", "-c", "exit 0")
	}
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
