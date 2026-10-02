//go:build windows

package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code Windows reports for a process that has not
// exited (STILL_ACTIVE == STATUS_PENDING). A process that genuinely exits with
// code 259 is indistinguishable from a running one via GetExitCodeProcess — an
// accepted edge case here, and it fails toward "alive", the safe direction (see
// alive and doc.go).
const stillActive = 259

const (
	restartManagerSessionKeyLength = 32
	restartManagerErrorMoreData    = 234
	restartManagerFileBatchSize    = 512
)

var (
	restartManagerDLL               = windows.NewLazySystemDLL("rstrtmgr.dll")
	restartManagerStartSession      = restartManagerDLL.NewProc("RmStartSession")
	restartManagerRegisterResources = restartManagerDLL.NewProc("RmRegisterResources")
	restartManagerGetList           = restartManagerDLL.NewProc("RmGetList")
	restartManagerEndSession        = restartManagerDLL.NewProc("RmEndSession")
)

type restartManagerUniqueProcess struct {
	ProcessID uint32
	StartTime windows.Filetime
}

type restartManagerProcessInfo struct {
	Process          restartManagerUniqueProcess
	AppName          [256]uint16
	ServiceShortName [64]uint16
	ApplicationType  uint32
	AppStatus        uint32
	SessionID        uint32
	Restartable      int32
}

// Tree on windows is the child pid plus the Job Object the whole descendant
// tree is terminated through. A zero job handle means no job is owned (a
// Configure-only caller that never routed through newTree), in which case kill
// degrades to terminating the lone pid.
type Tree struct {
	pid    int
	job    windows.Handle
	closed bool
}

type processIdentity struct {
	pid       int
	startTime time.Time
}

type terminationTarget struct {
	identity processIdentity
	handle   windows.Handle
}

type identityState int

const (
	identityGone identityState = iota
	identityStatePresent
	identityUnknown
)

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

// prepareStart suspends the child at creation so it cannot spawn descendants
// before newTree assigns it to the Job Object. Configure-only detached callers
// intentionally do not receive these flags.
func prepareStart(cmd *exec.Cmd) {
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW
}

// newTree creates a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, assigns
// the just-started child to it, and returns a Tree that terminates the whole
// job on kill. KILL_ON_JOB_CLOSE is the crash-safety guarantee the unix session
// gives for free: if the daemon dies, the OS closes the job handle and reaps
// every process still in the tree.
//
// Start creates the child suspended. Assignment therefore completes before the
// primary thread runs and can spawn descendants; resumeProcess releases it only
// after the Job Object owns the process.
func newTree(cmd *exec.Cmd) (*Tree, error) {
	t := &Tree{pid: cmd.Process.Pid}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("proc: create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
				windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK |
				windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK,
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
	defer func() { _ = windows.CloseHandle(proc) }()
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("proc: assign child %d to job: %w", t.pid, err)
	}
	t.job = job
	if err := resumeProcess(t.pid); err != nil {
		_ = windows.CloseHandle(job)
		t.job = 0
		return nil, err
	}

	// The seam has no explicit Close (a unix tree owns no resource), so release
	// the job handle when the Tree is dropped rather than leaking one handle per
	// stage. Closing the last handle also reaps any process still in the job
	// (KILL_ON_JOB_CLOSE) — the intended teardown, harmless once the tree has
	// already exited.
	runtime.SetFinalizer(t, func(t *Tree) { _ = windows.CloseHandle(t.job) })
	return t, nil
}

func resumeProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("proc: snapshot child threads: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	found := false
	resumed := false
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		found = true
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			if vanishedThread(openErr) {
				continue
			}
			return fmt.Errorf("proc: open child thread: %w", openErr)
		}
		previousSuspendCount, resumeErr := windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		if resumeErr != nil {
			return fmt.Errorf("proc: resume child thread: %w", resumeErr)
		}
		if previousSuspendCount > 0 {
			resumed = true
		}
	}
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("proc: enumerate child threads: %w", err)
	}
	if !found {
		return fmt.Errorf("proc: child process %d has no thread", pid)
	}
	if !resumed {
		return fmt.Errorf("proc: child process %d has no suspended thread", pid)
	}
	return nil
}

func vanishedThread(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}

// kill hard-terminates every process in the tree via TerminateJobObject, then
// releases the job handle promptly (the finalizer would otherwise hold it until
// GC — undesirable on the timeout path, exactly when freeing resources matters).
// WSL can broker a host process outside the job, so repeatedly snapshot and
// terminate descendants after closing the job to cover descendants created
// while the first snapshot was being processed.
func (t *Tree) kill() error {
	if t.closed {
		return nil
	}
	if t.job == 0 {
		t.closed = true
		return terminatePID(t.pid)
	}
	descendants, snapshotErr := snapshotDescendants(t.pid)
	targets := make([]terminationTarget, 0, len(descendants))
	// Pin verified process objects while the job is still active. WSL teardown
	// can reject new PROCESS_TERMINATE opens even though an escaped descendant
	// remains alive; a handle acquired first preserves both access and identity.
	for _, descendant := range descendants {
		target, err := openIdentityForTerminate(descendant)
		if err == nil && target.handle != 0 {
			targets = append(targets, target)
		}
	}
	err := windows.TerminateJobObject(t.job, 1)
	runtime.SetFinalizer(t, nil)
	_ = windows.CloseHandle(t.job)
	t.job = 0
	t.closed = true
	if err != nil {
		snapshotErr = errors.Join(snapshotErr, fmt.Errorf("proc: terminate job for %d: %w", t.pid, err))
	}
	targetErrs := make([]error, len(targets))
	for i := len(targets) - 1; i >= 0; i-- {
		targetErrs[i] = targets[i].terminate()
	}
	// WSL broker processes can continue rejecting termination briefly while
	// the job's members are being torn down. Keep retrying long enough for
	// that transition to settle without making Kill unbounded.
	deadline := time.Now().Add(5 * time.Second)
	// Only the LAST pass's failures are the caller's answer. Kill's contract
	// is about the state of the tree when it returns, and a descendant that
	// was mid-exit on one pass and gone on the next was terminated
	// successfully — joining the earlier pass's error anyway reported a
	// failure for a tree that is, in fact, dead (#4212).
	var terminateErr error
	for {
		terminateErr = nil
		for i := len(descendants) - 1; i >= 0; i-- {
			descendant := descendants[i]
			if descendant.startTime.IsZero() {
				continue
			}
			if err := terminateIdentity(descendant.pid, descendant.startTime); err != nil {
				terminateErr = errors.Join(terminateErr, err)
			}
		}
		if time.Now().After(deadline) {
			break
		}
		var snapshotErr2 error
		descendants, snapshotErr2 = snapshotDescendants(t.pid)
		if snapshotErr2 != nil {
			snapshotErr = errors.Join(snapshotErr, snapshotErr2)
			break
		}
		if len(descendants) == 0 {
			terminateErr = nil
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := range targets {
		if targetErrs[i] != nil &&
			identityStateForHandle(targets[i].handle, targets[i].identity.startTime) != identityGone {
			terminateErr = errors.Join(terminateErr, targetErrs[i])
		}
		targets[i].close()
	}
	return errors.Join(snapshotErr, terminateErr)
}

func snapshotDescendants(root int) ([]processIdentity, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("proc: snapshot process tree: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	type process struct {
		pid    int
		parent int
	}
	var processes []process
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		processes = append(processes, process{
			pid:    int(entry.ProcessID),
			parent: int(entry.ParentProcessID),
		})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("proc: enumerate process tree: %w", err)
	}

	children := make(map[int][]int)
	for _, process := range processes {
		children[process.parent] = append(children[process.parent], process.pid)
	}
	return identifyDescendants(root, children), nil
}

func identifyDescendants(root int, children map[int][]int) []processIdentity {
	return identifyDescendantsWithStartTime(root, children, startTime)
}

func identifyDescendantsWithStartTime(root int, children map[int][]int, readStartTime func(int) (time.Time, bool)) []processIdentity {
	// The walk is shared with the unix collectors and is cycle-safe: a
	// Windows entry's parent pid is its creator's pid at creation time and
	// survives that creator's exit, so a recycled pid can make the recorded
	// parentage point back into the subtree and an unguarded walk never
	// terminates (#3922).
	var descendants []processIdentity
	for _, pid := range collectDescendants(root, children) {
		// ParentProcessID is stale once the creator exits; an unreadable pid is
		// not safe proof that the original tree still owns that process.
		started, ok := readStartTime(pid)
		if !ok {
			continue
		}
		descendants = append(descendants, processIdentity{pid: pid, startTime: started})
	}
	return descendants
}

// terminateExitWait bounds how long a terminated process is waited on. The
// wait used to be INFINITE, which is a hang rather than a timeout: a process
// stuck in a driver or a kernel transition (a WSL broker's host process is the
// case this package meets) never signals, so Kill never returned and the whole
// test binary died on its ten-minute package timeout instead (#3922). A
// bounded wait turns that into a named error the caller can report.
const terminateExitWait = 30 * time.Second

// accessDeniedExitWait bounds the Windows teardown race where a process that is
// already exiting rejects new terminate access before it disappears.
const accessDeniedExitWait = 5 * time.Second

// terminatePID force-terminates a single process by pid — the degraded path when
// no Job Object was assigned, and for callers that identified their target by
// something other than a start time.
func terminatePID(pid int) error {
	return terminateIdentity(pid, time.Time{})
}

// terminateIdentity force-terminates pid, refusing to touch it unless it still
// names the process that started at started (the zero time waives the check).
//
// The identity check has to happen on the HANDLE, not before opening one
// (#4212). A pid is a reusable name: checking the start time and then opening
// the pid leaves a window in which the recorded process exits and an unrelated
// one — quite possibly a service this process has no business killing — takes
// the number, and the terminate lands on that. A handle pins the process
// object, so a start time read through it describes the process the terminate
// will actually reach.
//
// It also decides what an OpenProcess refusal MEANS, which the previous
// `alive(pid)` guard could not: alive() probes with OpenProcess too and fails
// toward "alive" on any error that is not a clean absent-pid, so an
// ERROR_ACCESS_DENIED was always reported as a live descendant this package
// had failed to kill — the reported flake, on a pid whose WSL-brokered owner
// had already recycled it. Re-reading the identity with query-only access
// separates the cases: a pid that no longer answers as the recorded process, or
// no longer answers at all, is not ours to kill and is not an error. A process
// that still answers as the recorded identity but refuses TERMINATE may also be
// in Windows job-object teardown; if it disappears within a bounded wait, Kill
// succeeded. If the same identity remains, the permission failure is genuine
// and is reported.
func terminateIdentity(pid int, started time.Time) error {
	if pid <= 0 {
		return nil
	}
	target, err := openIdentityForTerminate(processIdentity{pid: pid, startTime: started})
	if err != nil || target.handle == 0 {
		return err
	}
	defer target.close()
	return target.terminate()
}

func openIdentityForTerminate(identity processIdentity) (terminationTarget, error) {
	access := uint32(windows.PROCESS_TERMINATE | windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION)
	h, err := windows.OpenProcess(access, false, uint32(identity.pid))
	if err != nil {
		if !identity.startTime.IsZero() {
			state := identityStateForPID(identity.pid, identity.startTime)
			if state == identityGone {
				return terminationTarget{}, nil
			}
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) && waitForIdentityExit(identity.pid, identity.startTime, accessDeniedExitWait) {
				return terminationTarget{}, nil
			}
			state = identityStateForPID(identity.pid, identity.startTime)
			return terminationTarget{}, fmt.Errorf(
				"proc: open %d for terminate: recorded start %s, identity %s: %w",
				identity.pid, identity.startTime.Format(time.RFC3339Nano), state, err,
			)
		}
		return terminationTarget{}, fmt.Errorf("proc: open %d for terminate: %w", identity.pid, err)
	}
	target := terminationTarget{identity: identity, handle: h}
	if !identity.startTime.IsZero() && identityStateForHandle(h, identity.startTime) != identityStatePresent {
		target.close()
		return terminationTarget{}, nil
	}
	return target, nil
}

func (t terminationTarget) terminate() error {
	if err := windows.TerminateProcess(t.handle, 1); err != nil {
		if !t.identity.startTime.IsZero() {
			state := identityStateForHandle(t.handle, t.identity.startTime)
			if state == identityGone {
				return nil
			}
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) && waitForProcessExit(t.handle, accessDeniedExitWait) {
				return nil
			}
			state = identityStateForHandle(t.handle, t.identity.startTime)
			return fmt.Errorf(
				"proc: terminate %d: recorded start %s, identity %s: %w",
				t.identity.pid, t.identity.startTime.Format(time.RFC3339Nano), state, err,
			)
		}
		return fmt.Errorf("proc: terminate %d: %w", t.identity.pid, err)
	}
	status, err := windows.WaitForSingleObject(t.handle, uint32(terminateExitWait/time.Millisecond))
	if err != nil {
		return fmt.Errorf("proc: wait for %d to terminate: %w", t.identity.pid, err)
	}
	if status == uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("proc: process %d did not exit within %s of being terminated", t.identity.pid, terminateExitWait)
	}
	if status != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("proc: wait for %d to terminate returned status %#x", t.identity.pid, status)
	}
	return nil
}

func (t *terminationTarget) close() {
	if t.handle != 0 {
		_ = windows.CloseHandle(t.handle)
		t.handle = 0
	}
}

func (s identityState) String() string {
	switch s {
	case identityGone:
		return "gone"
	case identityStatePresent:
		return "present"
	default:
		return "unknown"
	}
}

func identityStateForPID(pid int, started time.Time) identityState {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return identityGone
		}
		return identityUnknown
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return identityStateForHandle(h, started)
}

func identityStateForHandle(h windows.Handle, started time.Time) identityState {
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return identityUnknown
	}
	if code != stillActive {
		return identityGone
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return identityUnknown
	}
	if time.Unix(0, creation.Nanoseconds()).Equal(started) {
		return identityStatePresent
	}
	return identityGone
}

func waitForIdentityExit(pid int, started time.Time, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if identityStateForPID(pid, started) == identityGone {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForProcessExit(h windows.Handle, timeout time.Duration) bool {
	status, err := windows.WaitForSingleObject(h, uint32(timeout/time.Millisecond))
	return err == nil && status == windows.WAIT_OBJECT_0
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
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

func killWorkspaceProcesses(workspace string) error {
	locked, err := workspaceLockingProcesses(workspace)
	if err != nil {
		return err
	}
	if len(locked) == 0 {
		return nil
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return fmt.Errorf("proc: snapshot build processes: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	processes := make(map[uint32]windows.ProcessEntry32)
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		processes[entry.ProcessID] = entry
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("proc: enumerate build processes: %w", err)
	}

	targets := make(map[uint32]bool)
	for pid, process := range processes {
		name := strings.ToLower(windows.UTF16ToString(process.ExeFile[:]))
		if locked[pid] && (name == "msbuild.exe" || name == "vbcscompiler.exe") {
			targets[pid] = true
		}
	}
	for {
		added := false
		for pid, process := range processes {
			if targets[process.ParentProcessID] && !targets[pid] {
				targets[pid] = true
				added = true
			}
		}
		if !added {
			break
		}
	}

	var errs []error
	for pid := range targets {
		if err := terminatePID(int(pid)); err != nil && alive(int(pid)) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func workspaceLockingProcesses(workspace string) (map[uint32]bool, error) {
	if workspace == "" {
		return nil, fmt.Errorf("proc: workspace path must not be empty")
	}
	var session uint32
	key := make([]uint16, restartManagerSessionKeyLength+1)
	result, _, _ := restartManagerStartSession.Call(
		uintptr(unsafe.Pointer(&session)),
		0,
		uintptr(unsafe.Pointer(&key[0])),
	)
	if result != 0 {
		return nil, fmt.Errorf("proc: start Restart Manager session: %w", syscall.Errno(result))
	}
	defer func() { _, _, _ = restartManagerEndSession.Call(uintptr(session)) }()

	files := make([]*uint16, 0, restartManagerFileBatchSize)
	register := func() error {
		if len(files) == 0 {
			return nil
		}
		result, _, _ := restartManagerRegisterResources.Call(
			uintptr(session),
			uintptr(len(files)),
			uintptr(unsafe.Pointer(&files[0])),
			0, 0, 0, 0,
		)
		files = files[:0]
		if result != 0 {
			return fmt.Errorf("proc: register workspace files with Restart Manager: %w", syscall.Errno(result))
		}
		return nil
	}
	err := filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return fmt.Errorf("proc: encode workspace path %q: %w", path, err)
		}
		files = append(files, name)
		if len(files) == restartManagerFileBatchSize {
			return register()
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("proc: inspect workspace locks: %w", err)
	}
	if err := register(); err != nil {
		return nil, err
	}

	var needed, count, rebootReasons uint32
	result, _, _ = restartManagerGetList.Call(
		uintptr(session),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&count)),
		0,
		uintptr(unsafe.Pointer(&rebootReasons)),
	)
	if result == 0 && needed == 0 {
		return nil, nil
	}
	if result != restartManagerErrorMoreData {
		return nil, fmt.Errorf("proc: query workspace lock holders: %w", syscall.Errno(result))
	}
	processes := make([]restartManagerProcessInfo, needed)
	count = needed
	result, _, _ = restartManagerGetList.Call(
		uintptr(session),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&count)),
		uintptr(unsafe.Pointer(&processes[0])),
		uintptr(unsafe.Pointer(&rebootReasons)),
	)
	if result != 0 {
		return nil, fmt.Errorf("proc: list workspace lock holders: %w", syscall.Errno(result))
	}
	locked := make(map[uint32]bool, count)
	for _, process := range processes[:count] {
		locked[process.Process.ProcessID] = true
	}
	return locked, nil
}
