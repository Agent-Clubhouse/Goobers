//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Tree on unix tracks its session-leader pid and descendants captured before a
// diagnostic signal can reparent them.
type Tree struct {
	pid         int
	descendants []processIdentity
}

type processIdentity struct {
	pid       int
	startTime time.Time
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

func prepareStart(*exec.Cmd) {}

// newTree captures the started process's pid, which — because configure made it
// a session leader — is also its process-group id.
func newTree(cmd *exec.Cmd) (*Tree, error) {
	return &Tree{pid: cmd.Process.Pid}, nil
}

func identifyProcesses(pids []int) []processIdentity {
	identities := make([]processIdentity, 0, len(pids))
	for _, pid := range pids {
		started, ok := StartTime(pid)
		if startTimeSupported && !ok {
			continue
		}
		identities = append(identities, processIdentity{pid: pid, startTime: started})
	}
	return identities
}

func (p processIdentity) signal(sig syscall.Signal) {
	if startTimeSupported {
		if p.startTime.IsZero() {
			return
		}
		started, ok := StartTime(p.pid)
		if !ok || !started.Equal(p.startTime) {
			return
		}
	}
	_ = syscall.Kill(p.pid, sig)
}

// kill snapshots descendants before terminating the process group so children
// that escaped into another session cannot be orphaned by their parent's exit.
func (t *Tree) kill() error {
	descendants := make(map[int]processIdentity, len(t.descendants))
	for _, process := range t.descendants {
		descendants[process.pid] = process
	}
	t.descendants = nil
	for _, process := range identifyProcesses(descendantPIDs(t.pid)) {
		descendants[process.pid] = process
	}
	groupErr := syscall.Kill(-t.pid, syscall.SIGKILL)
	for _, process := range descendants {
		process.signal(syscall.SIGKILL)
	}
	return groupErr
}

// requestDump SIGQUITs the whole process group so every Go process in it dumps
// its full goroutine trace and exits. Always supported on unix.
func (t *Tree) requestDump() (bool, error) {
	// Keep ownership of descendants that may exit the stage's session and be
	// reparented when their direct parent handles SIGQUIT.
	t.descendants = append(t.descendants, identifyProcesses(descendantPIDs(t.pid))...)
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

func killWorkspaceProcesses(string) error {
	return nil
}
