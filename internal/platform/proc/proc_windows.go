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
	pid int
	job windows.Handle
}

type processIdentity struct {
	pid       int
	startTime time.Time
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
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK,
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

// kill hard-terminates every process in the tree via TerminateJobObject, then
// releases the job handle promptly (the finalizer would otherwise hold it until
// GC — undesirable on the timeout path, exactly when freeing resources matters).
// WSL can broker a host process outside the job, so repeatedly snapshot and
// terminate descendants after closing the job to cover descendants created
// while the first snapshot was being processed.
func (t *Tree) kill() error {
	if t.job == 0 {
		return terminatePID(t.pid)
	}
	descendants, snapshotErr := snapshotDescendants(t.pid)
	err := windows.TerminateJobObject(t.job, 1)
	runtime.SetFinalizer(t, nil)
	_ = windows.CloseHandle(t.job)
	t.job = 0
	if err != nil {
		snapshotErr = errors.Join(snapshotErr, fmt.Errorf("proc: terminate job for %d: %w", t.pid, err))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		for i := len(descendants) - 1; i >= 0; i-- {
			descendant := descendants[i]
			if descendant.startTime.IsZero() {
				continue
			}
			current, ok := startTime(descendant.pid)
			if !ok || !current.Equal(descendant.startTime) {
				continue
			}
			if err := terminatePID(descendant.pid); err != nil && alive(descendant.pid) {
				snapshotErr = errors.Join(snapshotErr, err)
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
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return snapshotErr
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
	var descendants []processIdentity
	queue := append([]int(nil), children[root]...)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if started, ok := startTime(pid); ok {
			descendants = append(descendants, processIdentity{pid: pid, startTime: started})
		}
		queue = append(queue, children[pid]...)
	}
	return descendants, nil
}

// terminatePID force-terminates a single process by pid — the degraded path when
// no Job Object was assigned.
func terminatePID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("proc: open %d for terminate: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("proc: terminate %d: %w", pid, err)
	}
	status, err := windows.WaitForSingleObject(h, windows.INFINITE)
	if err != nil {
		return fmt.Errorf("proc: wait for %d to terminate: %w", pid, err)
	}
	if status != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("proc: wait for %d to terminate returned status %#x", pid, status)
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
