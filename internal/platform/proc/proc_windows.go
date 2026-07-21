//go:build windows

package proc

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code Windows reports for a process that has not
// exited (STILL_ACTIVE == STATUS_PENDING). A process that genuinely exits with
// code 259 is indistinguishable from a running one via GetExitCodeProcess — an
// accepted edge case here, and it fails toward "alive", the safe direction (see
// alive and doc.go).
const stillActive = 259

// Tree on windows is the child pid plus the Job Object the whole descendant
// tree is terminated through. A zero job handle means no job is owned (a
// Configure-only caller that never routed through newTree), in which case kill
// degrades to terminating the lone pid.
type Tree struct {
	pid int
	job windows.Handle
}

// configure detaches the child into its own process group so a console signal
// (Ctrl+C / Ctrl+Break) delivered to `goobers up` is not propagated into the
// stage — the windows analogue of the unix Setsid detach. Whole-tree teardown
// does not depend on the group: it uses the Job Object assigned in newTree.
// Idempotent, and it preserves any CreationFlags a caller already set (e.g.
// isolation flags), mirroring the unix configure's non-clobbering contract.
func configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// newTree creates a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, assigns
// the just-started child to it, and returns a Tree that terminates the whole
// job on kill. KILL_ON_JOB_CLOSE is the crash-safety guarantee the unix session
// gives for free: if the daemon dies, the OS closes the job handle and reaps
// every process still in the tree.
//
// Assignment happens immediately after Start rather than via a suspended
// create-then-resume (Go's exec does not expose the child's thread handle to
// resume). A grandchild spawned in the microseconds before assignment could
// escape the job — the same best-effort caveat the unix impl notes for a child
// that calls setsid itself.
func newTree(cmd *exec.Cmd) (*Tree, error) {
	t := &Tree{pid: cmd.Process.Pid}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("proc: create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("proc: set job kill-on-close limit: %w", err)
	}

	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(t.pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("proc: open child %d: %w", t.pid, err)
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("proc: assign child %d to job: %w", t.pid, err)
	}
	t.job = job

	// The seam has no explicit Close (a unix tree owns no resource), so release
	// the job handle when the Tree is dropped rather than leaking one handle per
	// stage. Closing the last handle also reaps any process still in the job
	// (KILL_ON_JOB_CLOSE) — the intended teardown, harmless once the tree has
	// already exited.
	runtime.SetFinalizer(t, func(t *Tree) { _ = windows.CloseHandle(t.job) })
	return t, nil
}

// kill hard-terminates every process in the tree via TerminateJobObject, then
// releases the job handle promptly (the finalizer would otherwise hold it until
// GC — undesirable on the timeout path, exactly when freeing resources matters).
// Without a job (Configure-only path) it best-effort terminates the lone pid.
func (t *Tree) kill() error {
	if t.job == 0 {
		return terminatePID(t.pid)
	}
	err := windows.TerminateJobObject(t.job, 1)
	runtime.SetFinalizer(t, nil)
	_ = windows.CloseHandle(t.job)
	t.job = 0
	if err != nil {
		return fmt.Errorf("proc: terminate job for %d: %w", t.pid, err)
	}
	return nil
}

// terminatePID force-terminates a single process by pid — the degraded path when
// no Job Object was assigned.
func terminatePID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("proc: open %d for terminate: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("proc: terminate %d: %w", pid, err)
	}
	return nil
}

// requestDump reports unsupported (supported=false): a Job Object cannot deliver
// a diagnostic-dump signal to its members — there is no SIGQUIT equivalent — so
// the caller proceeds straight to Kill, exactly as doc.go describes.
func (t *Tree) requestDump() (bool, error) {
	return false, nil
}

// alive reports whether pid names a live process, via OpenProcess +
// GetExitCodeProcess. Like the unix signal-0 probe it fails toward alive on an
// ambiguous result — an OpenProcess failure that is anything other than a clean
// "no such pid" (ERROR_INVALID_PARAMETER), or a process whose exit code cannot
// be read — because the caller is the worktree reaper, for which a false "dead"
// destroys a live run's worktree.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// A truly absent pid is reported as ERROR_INVALID_PARAMETER; any other
		// failure (e.g. ERROR_ACCESS_DENIED) means the process exists but is
		// not openable — fail toward alive.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false
		}
		return true
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
