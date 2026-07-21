package proc

import "os/exec"

// Tree is a handle to a started command and the descendant process tree it
// leads. Obtain one from Start; use Kill to terminate the whole tree at once.
//
// Its internals are platform-specific, so the struct itself is defined in each
// platform file rather than here: on unix the process-group id (equal to the
// session-leader pid) is the whole handle, while windows also carries the Job
// Object the tree is killed through. Every operation is a method on *Tree, so a
// windows build supplies both the fields and the method bodies with no call
// site changing.

// Configure arranges for cmd, once started, to lead its own controllable
// process tree — on unix a new session (Setsid), on windows a Job Object. It
// must be called before cmd.Start and is idempotent, so a caller that must set
// SysProcAttr fields for other reasons (e.g. network isolation) can Configure
// first and layer those on afterward. Callers that manage Start themselves and
// only need detachment (not tree signalling) may use Configure alone.
func Configure(cmd *exec.Cmd) {
	configure(cmd)
}

// Start configures cmd for tree ownership, starts it, and returns a Tree handle
// for later signalling or killing of the whole descendant tree. It is the spawn
// path for every caller that will tree-kill on timeout or cancel.
//
// The returned error is exactly cmd.Start's on unix; on windows it also covers
// a failure to assign the started process to its Job Object. Callers keep their
// own error wrapping around this call.
func Start(cmd *exec.Cmd) (*Tree, error) {
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return newTree(cmd)
}

// Kill hard-terminates every process in the tree — on unix SIGKILL to the
// process group, on windows TerminateJobObject. It is best-effort: a descendant
// that escaped the tree (e.g. via its own setsid) may survive, exactly as
// before this abstraction existed.
func (t *Tree) Kill() error {
	return t.kill()
}

// RequestDump asks every process in the tree to emit diagnostics and exit. On
// unix it sends SIGQUIT, which makes each Go process dump all goroutine stacks
// (regardless of GOTRACEBACK) and exit — the self-diagnosing artifact a
// timed-out stage leaves behind before it is force-killed.
//
// supported reports whether the platform can deliver such a request; a windows
// Job Object cannot signal its members, so it returns false and the caller
// proceeds straight to Kill. The error is diagnostic-only — a caller should
// still fall back to Kill regardless.
func (t *Tree) RequestDump() (supported bool, err error) {
	return t.requestDump()
}

// Alive reports whether pid names a live process. On unix it is a signal-0
// probe. It fails toward alive on an ambiguous probe (see doc.go): the caller
// is the worktree reaper, for which a false "dead" is destructive.
func Alive(pid int) bool {
	return alive(pid)
}
