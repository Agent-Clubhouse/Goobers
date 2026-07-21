//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Tree on unix is fully identified by its session-leader pid, which — because
// configure made the child a session leader — is also its process-group id.
type Tree struct {
	pid int
}

// configure puts the child in a NEW SESSION (Setsid), not merely a new process
// group (Setpgid). A bare Setpgid child of a `goobers up` in the foreground of
// an interactive terminal is a background process group on that controlling
// terminal, which the kernel STOPS (SIGTTOU/SIGTTIN, state T, zero CPU) the
// moment it touches terminal state — the "local-ci hang" (#846). Setsid
// detaches the controlling terminal entirely so job control cannot freeze it.
// The session leader's process-group id equals its pid, so killing the negative
// pid below signals the whole tree.
//
// Idempotent, and it never clobbers other SysProcAttr fields a caller layered
// on (e.g. the network-isolation Cloneflags in executor/network_linux.go).
func configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// newTree captures the started process's pid, which — because configure made it
// a session leader — is also its process-group id.
func newTree(cmd *exec.Cmd) (*Tree, error) {
	return &Tree{pid: cmd.Process.Pid}, nil
}

// kill SIGKILLs the whole process group (negative pid), not just the direct
// child, so a runaway subprocess tree cannot outlive the stage.
func (t *Tree) kill() error {
	return syscall.Kill(-t.pid, syscall.SIGKILL)
}

// requestDump SIGQUITs the whole process group so every Go process in it dumps
// its full goroutine trace and exits. Always supported on unix.
func (t *Tree) requestDump() (bool, error) {
	return true, syscall.Kill(-t.pid, syscall.SIGQUIT)
}

// alive probes pid with signal 0, which checks liveness without actually
// signalling. Best-effort: PID reuse after a reboot can in principle produce a
// false "alive" for an unrelated process, an accepted limitation at V0 (#142).
//
// It fails toward alive: a signal-0 that returns EPERM means the process EXISTS
// but belongs to another user (unreachable for the daemon's own same-user
// subprocesses, but the safe answer regardless) — reported alive, because the
// caller is the worktree reaper and a false "dead" reaps a live run's worktree.
// Only an unambiguous "no such process" (ESRCH) counts as dead.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	switch err := process.Signal(syscall.Signal(0)); {
	case err == nil:
		return true
	case errors.Is(err, syscall.EPERM):
		return true
	default:
		return false
	}
}
