//go:build unix

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// probeAlive is the test's own liveness check, independent of the package's
// Alive, so a bug in Alive can't hide a surviving process. It reports true for
// EPERM (exists, not ours) as well as a clean signal-0.
func probeAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func readPID(t *testing.T, path string) (int, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return pid, true
}

// TestKillTreeReapsGrandchild is the key semantic (#623): a harness spawns its
// own subprocess trees, so KillTree must leave no descendant alive — not just
// the direct child. The fixture is a parent shell that backgrounds a child
// shell that backgrounds a grandchild sleep; all three inherit the session
// configure sets up, so one group kill must take them all.
func TestKillTreeReapsGrandchild(t *testing.T) {
	dir := t.TempDir()
	// Outer sh records its own pid, then backgrounds an inner sh that records
	// its pid and a grandchild sleep's pid. Every process references $PIDDIR
	// from the environment, so no fragile nested quoting is needed.
	script := `echo $$ > "$PIDDIR/parent.pid"
sh -c 'echo $$ > "$PIDDIR/child.pid"; sleep 300 & echo $! > "$PIDDIR/grandchild.pid"; wait' &
wait`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PIDDIR="+dir)

	tree, err := Start(cmd)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var parent, child, grandchild int
	if !waitUntil(t, 5*time.Second, func() bool {
		var ok1, ok2, ok3 bool
		parent, ok1 = readPID(t, filepath.Join(dir, "parent.pid"))
		child, ok2 = readPID(t, filepath.Join(dir, "child.pid"))
		grandchild, ok3 = readPID(t, filepath.Join(dir, "grandchild.pid"))
		return ok1 && ok2 && ok3
	}) {
		_ = tree.Kill()
		_ = cmd.Wait()
		t.Fatalf("processes never recorded their pids (parent=%d child=%d grandchild=%d)", parent, child, grandchild)
	}

	if !probeAlive(grandchild) {
		t.Fatalf("grandchild %d not alive before kill", grandchild)
	}

	if err := tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	// Reap the direct child so it doesn't linger as a zombie in this test
	// process; the re-parented child/grandchild are reaped by init.
	_ = cmd.Wait()

	for _, p := range []struct {
		name string
		pid  int
	}{{"parent", parent}, {"child", child}, {"grandchild", grandchild}} {
		p := p
		if !waitUntil(t, 5*time.Second, func() bool { return !probeAlive(p.pid) }) {
			// Best-effort cleanup of any survivor before failing.
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
			t.Errorf("%s process %d survived KillTree", p.name, p.pid)
		}
	}
}

// TestRequestDumpSignalsTree verifies the SIGQUIT dump path: it reports
// supported on unix and actually terminates the tree (SIGQUIT's default
// disposition), so shell.go's timeout dump-then-wait behaves as before.
func TestRequestDumpSignalsTree(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	tree, err := Start(cmd)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid

	supported, err := tree.RequestDump()
	if err != nil {
		t.Fatalf("RequestDump: %v", err)
	}
	if !supported {
		t.Fatalf("RequestDump reported unsupported on unix")
	}

	waitDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		_ = tree.Kill()
		<-waitDone
		t.Fatalf("process %d did not exit after RequestDump", pid)
	}
}

func TestAliveSelfIsAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatalf("Alive(self) = false, want true")
	}
}

func TestAliveNonPositivePIDIsDead(t *testing.T) {
	for _, pid := range []int{0, -1, -12345} {
		if Alive(pid) {
			t.Errorf("Alive(%d) = true, want false", pid)
		}
	}
}

// TestAliveExitedProcessIsDead spawns and fully reaps a process, then checks it
// reads as dead. There is an inherent PID-reuse race (documented on alive), but
// the window between reaping and the probe is tiny.
func TestAliveExitedProcessIsDead(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if Alive(pid) {
		t.Errorf("Alive(%d) = true for a reaped process, want false", pid)
	}
}

// TestAliveFailsTowardAlive covers the probe-error case: pid 1 (init/launchd)
// exists but a non-root probe gets EPERM — Alive must report it alive, never
// dead, because a false "dead" is the destructive direction for the reaper.
// (As root the probe returns nil instead; either way the process exists and the
// answer is alive.)
func TestAliveFailsTowardAlive(t *testing.T) {
	if !Alive(1) {
		t.Errorf("Alive(1) = false, want true (must fail toward alive on an ambiguous probe)")
	}
}
