//go:build integration && linux

package childpod

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Run only as the entrypoint of a disposable private PID namespace, with no
// other workload in the container. The production guard must still verify PID 1.
func TestIntegrationPID1QuiescesDetachedWriter(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_PID1_TEST")
	testdep.Require(t, "sh", "sleep")
	if err := VerifyEntrypoint(); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Fatal("qualification must use the non-root worker identity")
	}
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop-and-reap", true: "cancelled-proof"}[cancelled], func(t *testing.T) {
			pid, heartbeat := detachedQualificationWriter(t)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := Quiesce(ctx); err != nil {
					t.Error("cleanup could not stop detached writer", err)
				}
			})
			if cancelled {
				before := heartbeatBytes(t, heartbeat)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := Quiesce(ctx); !errors.Is(err, context.Canceled) {
					t.Fatal("cancelled observation claimed stopped writers", err)
				}
				waitHeartbeat(t, heartbeat, before+1)
			}
			if err := Quiesce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("detached writer was not reaped", pid, err)
			}
			stopped := heartbeatBytes(t, heartbeat)
			time.Sleep(100 * time.Millisecond)
			if heartbeatBytes(t, heartbeat) != stopped {
				t.Fatal("workspace writer survived confirmed quiescence")
			}
		})
	}
}

func detachedQualificationWriter(t *testing.T) (int, string) {
	t.Helper()
	dir := t.TempDir()
	pidPath, heartbeat := filepath.Join(dir, "writer.pid"), filepath.Join(dir, "writes")
	// The outer shell creates a new session, then exits after forking a writer.
	// Its orphan is adopted by our PID 1; ordinary process-group waiting would
	// miss this writer. Redirecting output keeps no inherited test pipe open.
	command := exec.Command("sh", "-c", `sh -c 'echo $$ > "$1"; while :; do printf x >> "$2"; sleep 0.02; done' writer "$1" "$2" >/dev/null 2>&1 &`, "launcher", pidPath, heartbeat)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("start detached writer", err, string(output))
	}
	waitHeartbeat(t, heartbeat, 2)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatal("invalid writer PID", pid, err)
	}
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil || !strings.Contains(string(status), "PPid:\t1\n") {
		t.Fatal("writer did not detach into supervisor custody", string(status), err)
	}
	return pid, heartbeat
}

func heartbeatBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func waitHeartbeat(t *testing.T, path string, minimum int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() >= minimum {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("detached writer did not produce its heartbeat")
}
