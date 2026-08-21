//go:build linux

package proc

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestAliveReportsUnreapedZombieDead is the #3399 regression. The observed
// shape: a stage descendant reparents onto a daemon that never wait()s for it,
// so the pid stays allocated and the signal-0 probe answers "alive" for the
// life of the pod — a PERMANENT false-alive, which pins the run's worktree
// against the reaper forever.
//
// The fixture reproduces exactly that state (a child nobody waits for) and
// asserts both halves: the old probe still says the process exists, and Alive
// nevertheless reports it dead.
func TestAliveReportsUnreapedZombieDead(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	// Never Wait during the test — an unreaped child is the whole point.
	t.Cleanup(func() { _ = cmd.Wait() })

	if !waitUntil(t, 5*time.Second, func() bool { return zombie(pid) }) {
		t.Fatalf("child %d never entered the zombie state", pid)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("signal-0 probe of zombie %d returned %v; the fixture no longer reproduces the false-alive", pid, err)
	}
	if Alive(pid) {
		t.Errorf("Alive(%d) = true for an unreaped zombie, want false", pid)
	}
}

// TestZombieOnlyReportsPositiveEvidence pins the fail-toward-alive contract
// doc.go requires: zombie may answer true only for a /proc entry it actually
// read as state 'Z'. Anything unknown must leave Alive's answer alone.
func TestZombieOnlyReportsPositiveEvidence(t *testing.T) {
	if zombie(os.Getpid()) {
		t.Errorf("zombie(self) = true, want false")
	}
	for _, pid := range []int{0, -1, -12345} {
		if zombie(pid) {
			t.Errorf("zombie(%d) = true for an unreadable /proc entry, want false", pid)
		}
	}

	running := exec.Command("sleep", "300")
	if err := running.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = running.Process.Kill()
		_ = running.Wait()
	})
	if zombie(running.Process.Pid) {
		t.Errorf("zombie(%d) = true for a running process, want false", running.Process.Pid)
	}
	if !Alive(running.Process.Pid) {
		t.Errorf("Alive(%d) = false for a running process, want true", running.Process.Pid)
	}
}
