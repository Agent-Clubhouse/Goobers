//go:build windows

package proc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestVanishedThreadOnlyAcceptsInvalidParameter(t *testing.T) {
	if !vanishedThread(windows.ERROR_INVALID_PARAMETER) {
		t.Fatal("vanished thread error was not recognized")
	}
	if vanishedThread(windows.ERROR_ACCESS_DENIED) {
		t.Fatal("access denied was treated as a vanished thread")
	}
	if vanishedThread(errors.New("invalid parameter")) {
		t.Fatal("untyped error was treated as a vanished thread")
	}
}

func TestIdentityStateDistinguishesPresentAndGone(t *testing.T) {
	selfStarted, ok := startTime(os.Getpid())
	if !ok {
		t.Fatal("current process start time was not readable")
	}
	if got := identityStateForPID(os.Getpid(), selfStarted); got != identityStatePresent {
		t.Fatalf("identityStateForPID(current) = %v, want present", got)
	}
	if got := identityStateForPID(os.Getpid(), selfStarted.Add(time.Nanosecond)); got != identityGone {
		t.Fatalf("identityStateForPID(current with different start) = %v, want gone", got)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(), "GOOBERS_PROC_HELPER_ROLE=short")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var started time.Time
	ok = false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		started, ok = startTime(cmd.Process.Pid)
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !ok {
		t.Fatal("helper process start time was not readable")
	}
	if got := identityStateForPID(cmd.Process.Pid, started); got != identityGone {
		t.Fatalf("identityStateForPID(exited) = %v, want gone", got)
	}
}

func TestStartAttachesBeforeChildExecutes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(),
		"GOOBERS_PROC_HELPER_ROLE=marker",
		"GOOBERS_PROC_HELPER_MARKER="+marker,
	)
	Configure(cmd)
	prepareStart(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // Intentional pre-resume window proves suspended child containment.
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child executed before Job Object attachment: %v", err)
	}
	tree, err := newTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tree.Kill()
		_ = cmd.Wait()
	}()

	// Allow a minute for process startup on heavily contended Windows runners.
	// The helper's runtime is longer than this deadline so a slow start cannot
	// turn into a misleading timeout after the child exits.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not execute after Job Object attachment and resume")
		}
		time.Sleep(10 * time.Millisecond) // Polling interval for the child process marker.
	}
}

func TestKillTerminatesJobDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child.pid")
	grandchildMarker := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(),
		"GOOBERS_PROC_HELPER_ROLE=root",
		"GOOBERS_PROC_HELPER_PID="+marker,
		"GOOBERS_PROC_HELPER_GRANDCHILD="+grandchildMarker,
	)
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		_ = tree.Kill()
		_ = cmd.Wait()
	}()

	// Leave margin below the helpers' 30-second sleep while allowing process
	// startup under contention on shared Windows runners.
	const helperReadyTimeout = 25 * time.Second
	var childPID int
	deadline := time.Now().Add(helperReadyTimeout)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(marker)
		if readErr == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("descendant did not record its pid")
	}
	if !Alive(childPID) {
		t.Fatalf("descendant %d exited before tree termination", childPID)
	}
	var grandchildPID int
	deadline = time.Now().Add(helperReadyTimeout)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(grandchildMarker)
		if readErr == nil {
			grandchildPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if grandchildPID == 0 {
		t.Fatal("grandchild did not record its pid")
	}
	if !Alive(grandchildPID) {
		t.Fatalf("grandchild %d exited before tree termination", grandchildPID)
	}

	if err := tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("parent unexpectedly exited successfully after Kill")
	}
	deadline = time.Now().Add(5 * time.Second)
	for Alive(childPID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if Alive(childPID) {
		t.Fatalf("descendant %d survived tree termination", childPID)
	}
	if Alive(grandchildPID) {
		t.Fatalf("grandchild %d survived tree termination", grandchildPID)
	}
}

func TestKillIsIdempotentAfterJobClose(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(), "GOOBERS_PROC_HELPER_ROLE=marker", "GOOBERS_PROC_HELPER_MARKER="+filepath.Join(t.TempDir(), "started"))
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tree.Kill()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if err := tree.Kill(); err != nil {
		t.Fatalf("first Kill: %v", err)
	}
	_ = cmd.Wait()
	if err := tree.Kill(); err != nil {
		t.Fatalf("second Kill: %v", err)
	}
}

func TestIdentifyDescendantsIgnoresUnreadableParentage(t *testing.T) {
	started := time.Unix(123, 0)
	got := identifyDescendantsWithStartTime(10, processLifetime{}, map[int][]int{
		10: {20, 30},
		20: {40},
	}, func(pid int) (time.Time, bool) {
		if pid == 20 {
			return started, true
		}
		return time.Time{}, false
	})
	if len(got) != 1 || got[0].pid != 20 || !got[0].startTime.Equal(started) {
		t.Fatalf("identifyDescendantsWithStartTime = %+v, want only pid 20", got)
	}
}

// A recycled pid makes processes its previous holder created read as children
// of the tree; one that started before its recorded parent is not ours, and
// neither is anything below it or below a pid with no readable identity (#6744).
func TestIdentifyDescendantsRejectsStaleParentage(t *testing.T) {
	base := time.Unix(1000, 0)
	starts := map[int]time.Time{
		10: base,
		20: base.Add(time.Second),      // real child of root
		30: base.Add(-time.Hour),       // stale: predates root
		31: base.Add(time.Second),      // below a stale entry
		40: base.Add(2 * time.Second),  // real grandchild of 20
		41: base.Add(time.Millisecond), // stale: predates 20
		60: base.Add(3 * time.Second),  // below unreadable 50
		70: base,                       // started in the same tick as root
	}
	got := identifyDescendantsWithStartTime(10, processLifetime{start: base}, map[int][]int{
		10: {20, 30, 50, 70},
		20: {40, 41},
		30: {31},
		40: {20, 10}, // cyclic parentage from recycled pids (#3922)
		50: {60},
	}, func(pid int) (time.Time, bool) {
		started, ok := starts[pid]
		return started, ok
	})
	assertDescendantPIDs(t, got, starts, 20, 70, 40)
}

// Kill re-snapshots after terminating the root. Its real orphans were created
// before it exited; a recorded child that started afterwards was created by a
// later holder of its pid.
func TestIdentifyDescendantsRejectsChildrenAfterRootExit(t *testing.T) {
	base := time.Unix(1000, 0)
	exited := base.Add(time.Minute)
	starts := map[int]time.Time{
		20: base.Add(time.Second),       // orphan of the original root
		30: exited.Add(time.Second),     // created by a later holder of pid 10
		40: exited.Add(time.Hour),       // grandchild through the orphan
		50: exited,                      // created in the root's final tick
		60: exited.Add(2 * time.Second), // below the later holder's child
	}
	got := identifyDescendantsWithStartTime(10, processLifetime{start: base, exit: exited}, map[int][]int{
		10: {20, 30, 50},
		20: {40},
		30: {60},
	}, func(pid int) (time.Time, bool) {
		started, ok := starts[pid]
		return started, ok
	})
	assertDescendantPIDs(t, got, starts, 20, 50, 40)
}

func assertDescendantPIDs(t *testing.T, got []processIdentity, starts map[int]time.Time, want ...int) {
	t.Helper()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		ok = got[i].pid == want[i] && got[i].startTime.Equal(starts[want[i]])
	}
	if !ok {
		t.Fatalf("identifyDescendantsWithStartTime = %+v, want pids %v", got, want)
	}
}

func TestProcessTreeHelper(t *testing.T) {
	role := os.Getenv("GOOBERS_PROC_HELPER_ROLE")
	if role == "" {
		return
	}
	pidMarker := os.Getenv("GOOBERS_PROC_HELPER_PID")
	grandchildMarker := os.Getenv("GOOBERS_PROC_HELPER_GRANDCHILD")
	switch role {
	case "wsl":
		runWSLProcessHelper(t, pidMarker)
		return
	case "marker":
		if err := os.WriteFile(os.Getenv("GOOBERS_PROC_HELPER_MARKER"), []byte("started"), 0600); err != nil {
			t.Fatal(err)
		}
	case "short":
		time.Sleep(2 * time.Second)
		return
	case "root":
		cmd := exec.Command(os.Args[0], "-test.run=TestProcessTreeHelper")
		cmd.Env = append(os.Environ(), "GOOBERS_PROC_HELPER_ROLE=child")
		if os.Getenv("GOOBERS_PROC_HELPER_BREAKAWAY") == "1" {
			cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pidMarker, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
	case "child":
		cmd := exec.Command(os.Args[0], "-test.run=TestProcessTreeHelper")
		cmd.Env = append(os.Environ(), "GOOBERS_PROC_HELPER_ROLE=grandchild")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(grandchildMarker, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(90 * time.Second)
}

// runWSLProcessHelper keeps the real WSL launcher below a native test process.
// PowerShell startup is not part of the process-containment contract; previous
// failures exhausted readiness with that launcher alive and no PID or output.
// Write milestones directly: testing.T buffers would be lost when the parent
// deliberately terminates this helper before its test returns.
func runWSLProcessHelper(t *testing.T, marker string) {
	t.Helper()
	var cmd *exec.Cmd
	var failures []string
	for attempt := 1; cmd == nil && attempt <= wslLauncherAttempts; attempt++ {
		var err error
		if cmd, err = startReadyWSLGuest(attempt); err != nil {
			_, _ = os.Stderr.WriteString("WSL helper: " + err.Error() + "\n")
			failures = append(failures, err.Error())
			if attempt < wslLauncherAttempts {
				time.Sleep(time.Duration(attempt) * time.Second) // Back off before relaunching a launcher that failed to initialize.
			}
		}
	}
	if cmd == nil {
		t.Fatalf("WSL launcher never reached guest readiness in %d attempts: %s", wslLauncherAttempts, strings.Join(failures, "; "))
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if err := os.WriteFile(marker, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatalf("record WSL launcher PID: %v", err)
	}
	_, _ = os.Stderr.WriteString("WSL helper: recorded launcher PID\n")
	// An exit after guest readiness is a real early exit, even with a zero
	// exit code: the parent must observe a live WSL subtree before testing
	// termination.
	err := cmd.Wait()
	t.Fatalf("WSL launcher exited before tree termination: %v", err)
}

const (
	// wsl.exe can die during its own startup, before it ever reaches the WSL
	// service (hosted runners have reported STATUS_DLL_INIT_FAILED,
	// 0xc0000142, within milliseconds of launch: #6157, #6158). That is
	// launcher setup, not the tree under test, so a bounded number of
	// relaunches is allowed before the guest proves it is running.
	wslLauncherAttempts = 3
	wslGuestReady       = "goobers-wsl-guest-ready"
	wslReadyTimeout     = 60 * time.Second
)

// startReadyWSLGuest launches wsl.exe and returns once the guest command has
// printed its readiness line, so the launcher PID is only published for a WSL
// subtree that actually started. A launcher that exits first is reaped and
// reported so the caller can relaunch it.
func startReadyWSLGuest(attempt int) (*exec.Cmd, error) {
	_, _ = fmt.Fprintf(os.Stderr, "WSL helper: starting wsl.exe (attempt %d/%d)\n", attempt, wslLauncherAttempts)
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create WSL launcher stdout pipe: %w", err)
	}
	cmd := exec.Command("wsl.exe", "-e", "sh", "-c", "echo "+wslGuestReady+"; exec sleep 90")
	cmd.Stdout = stdoutWriter
	cmd.Stderr = os.Stderr
	startErr := cmd.Start()
	_ = stdoutWriter.Close()
	if startErr != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("start WSL launcher: %w", startErr)
	}
	_, _ = fmt.Fprintf(os.Stderr, "WSL helper: started launcher PID %d\n", cmd.Process.Pid)
	// Echo the launcher's output for diagnostics. The read ends at the
	// readiness line or when the launcher, the only writer, exits.
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		_, _ = os.Stdout.WriteString(scanner.Text() + "\n")
		if strings.TrimSpace(scanner.Text()) == wslGuestReady {
			_, _ = os.Stderr.WriteString("WSL helper: guest is running\n")
			// Keep draining so the launcher never writes into a closed pipe.
			go func() {
				_, _ = io.Copy(os.Stdout, stdout)
				_ = stdout.Close()
			}()
			return cmd, nil
		}
	}
	_ = stdout.Close()
	exitErr := errors.Join(cmd.Wait(), scanner.Err())
	if exitErr == nil {
		exitErr = errors.New("exit status 0")
	}
	return nil, fmt.Errorf("launcher PID %d exited before guest readiness (attempt %d/%d): %w",
		cmd.Process.Pid, attempt, wslLauncherAttempts, exitErr)
}

func TestKillTerminatesEscapedDescendants(t *testing.T) {
	childMarker := filepath.Join(t.TempDir(), "child.pid")
	grandchildMarker := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := exec.Command(os.Args[0], "-test.run=TestProcessTreeHelper")
	cmd.Env = append(os.Environ(),
		"GOOBERS_PROC_HELPER_ROLE=root",
		"GOOBERS_PROC_HELPER_PID="+childMarker,
		"GOOBERS_PROC_HELPER_GRANDCHILD="+grandchildMarker,
		"GOOBERS_PROC_HELPER_BREAKAWAY=1",
	)
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tree.Kill()
		_ = cmd.Wait()
	}()

	readPID := func(path string) int {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return 0
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if parseErr != nil {
			return 0
		}
		return pid
	}
	deadline := time.Now().Add(5 * time.Second)
	var childPID, grandchildPID int
	for time.Now().Before(deadline) && (childPID == 0 || grandchildPID == 0) {
		childPID = readPID(childMarker)
		grandchildPID = readPID(grandchildMarker)
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 || grandchildPID == 0 {
		t.Fatal("escaped process tree did not record both descendant pids")
	}
	if !Alive(childPID) || !Alive(grandchildPID) {
		t.Fatal("escaped descendants exited before tree termination")
	}

	originalOpenProcess := openProcessForTerminate
	originalTerminateJob := terminateTreeJob
	jobTerminated := false
	deniedOpens := 0
	openProcessForTerminate = func(access uint32, inheritHandle bool, pid uint32) (windows.Handle, error) {
		if jobTerminated {
			deniedOpens++
			return 0, windows.ERROR_ACCESS_DENIED
		}
		return originalOpenProcess(access, inheritHandle, pid)
	}
	terminateTreeJob = func(job windows.Handle, exitCode uint32) error {
		err := originalTerminateJob(job, exitCode)
		jobTerminated = true
		return err
	}
	defer func() {
		openProcessForTerminate = originalOpenProcess
		terminateTreeJob = originalTerminateJob
	}()

	if err := tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if deniedOpens == 0 {
		t.Fatal("Kill did not exercise the post-job access-denied path")
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("parent unexpectedly exited successfully after Kill")
	}
	deadline = time.Now().Add(5 * time.Second)
	for (Alive(childPID) || Alive(grandchildPID)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if Alive(childPID) || Alive(grandchildPID) {
		t.Fatalf("escaped descendants %d and %d survived tree termination", childPID, grandchildPID)
	}
}

func TestKillTerminatesWSLDescendants(t *testing.T) {
	if os.Getenv("GOOBERS_RUN_WSL_INTEGRATION_TEST") != "1" {
		t.Skip("set GOOBERS_RUN_WSL_INTEGRATION_TEST=1 to run the disruptive WSL integration test")
	}
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		t.Fatalf("WSL integration was explicitly required but wsl.exe is unavailable: %v", err)
	}

	marker := filepath.Join(t.TempDir(), "wsl.pid")
	// Keep helper startup errors rather than reducing every setup failure to a
	// missing PID. Read only after the launcher has stopped, and bound CI output.
	launcherLogPath := filepath.Join(t.TempDir(), "wsl-launcher.log")
	launcherLog, err := os.Create(launcherLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = launcherLog.Close()
		if !t.Failed() {
			return
		}
		log, openErr := os.Open(launcherLogPath)
		if openErr != nil {
			t.Logf("open WSL launcher diagnostics: %v", openErr)
			return
		}
		defer func() { _ = log.Close() }()
		data, readErr := io.ReadAll(io.LimitReader(log, 16*1024))
		t.Logf("WSL launcher stdout/stderr (first 16 KiB, read error %v):\n%s", readErr, data)
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(),
		"GOOBERS_PROC_HELPER_ROLE=wsl",
		"GOOBERS_PROC_HELPER_PID="+marker,
	)
	cmd.Stdout = launcherLog
	cmd.Stderr = launcherLog
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		killErr := tree.Kill()
		waitErr := cmd.Wait()
		if t.Failed() {
			t.Logf("WSL launcher cleanup: kill=%v, wait=%v", killErr, waitErr)
		}
	}()

	var wslPID int
	var markerData []byte
	var markerErr error
	// The helper publishes the PID only after the guest is running, which can
	// include a cold distro boot and launcher relaunches (#6157).
	deadline := time.Now().Add(wslReadyTimeout)
	for time.Now().Before(deadline) {
		markerData, markerErr = os.ReadFile(marker)
		if markerErr == nil {
			wslPID, markerErr = strconv.Atoi(strings.TrimSpace(string(markerData)))
			if markerErr == nil && wslPID > 0 {
				break
			}
		}
		if !Alive(cmd.Process.Pid) {
			t.Fatalf("WSL launcher %d exited before recording a valid PID: marker=%q, error=%v", cmd.Process.Pid, markerData, markerErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if wslPID <= 0 {
		t.Fatalf("WSL process did not record its pid within %s: launcher=%d alive=%t, marker=%q, error=%v", wslReadyTimeout, cmd.Process.Pid, Alive(cmd.Process.Pid), markerData, markerErr)
	}
	wslStart, ok := startTime(wslPID)
	if !ok {
		t.Fatalf("WSL process %d exited before tree termination", wslPID)
	}
	// Starting wsl.exe and its brokered descendants are asynchronous. Wait for
	// both within the original readiness deadline; do not rely on incidental
	// PowerShell/marker overhead to give the WSL broker time to start.
	var guestDescendants []processIdentity
	for time.Now().Before(deadline) {
		var snapshotErr error
		guestDescendants, snapshotErr = snapshotDescendants(wslPID, processLifetime{start: wslStart})
		if snapshotErr != nil {
			t.Fatalf("snapshot WSL descendants: %v", snapshotErr)
		}
		if len(guestDescendants) > 0 {
			break
		}
		if !Alive(wslPID) {
			t.Fatalf("WSL process %d exited before recording descendants", wslPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(guestDescendants) == 0 {
		t.Fatal("WSL launcher did not retain a host or guest descendant")
	}

	var brokeredDescendant processIdentity
	for brokeredDescendant.pid == 0 && time.Now().Before(deadline) {
		var snapshotErr error
		guestDescendants, snapshotErr = snapshotDescendants(wslPID, processLifetime{start: wslStart})
		if snapshotErr != nil {
			t.Fatalf("snapshot WSL descendants: %v", snapshotErr)
		}
		for _, descendant := range guestDescendants {
			inJob, membershipErr := processInJob(descendant.pid, tree.job)
			if membershipErr != nil {
				state := identityStateForPID(descendant.pid, descendant.startTime)
				if state == identityGone {
					continue
				}
				t.Fatalf(
					"query WSL descendant %d job membership: recorded start %s, identity %s: %v",
					descendant.pid, descendant.startTime.Format(time.RFC3339Nano), state, membershipErr,
				)
			}
			if !inJob {
				brokeredDescendant = descendant
				break
			}
		}
		if brokeredDescendant.pid == 0 {
			if !Alive(wslPID) {
				t.Fatalf("WSL process %d exited before retaining a brokered descendant", wslPID)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if brokeredDescendant.pid == 0 {
		t.Fatal("WSL launcher did not retain a brokered descendant outside the job")
	}

	if err := tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("parent unexpectedly exited successfully after Kill")
	}
	deadline = time.Now().Add(5 * time.Second)
	for Alive(wslPID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if Alive(wslPID) {
		t.Fatalf("WSL process %d survived tree termination", wslPID)
	}
	var brokeredState identityState
	for brokeredState = identityStateForPID(brokeredDescendant.pid, brokeredDescendant.startTime); brokeredState != identityGone && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		brokeredState = identityStateForPID(brokeredDescendant.pid, brokeredDescendant.startTime)
	}
	if brokeredState != identityGone {
		t.Fatalf(
			"WSL host descendant %d did not reach gone after tree termination: recorded start %s, identity %s",
			brokeredDescendant.pid, brokeredDescendant.startTime.Format(time.RFC3339Nano), brokeredState,
		)
	}
}

func processInJob(pid int, job windows.Handle) (bool, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(process) }()

	var result int32
	isProcessInJob := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	ok, _, callErr := isProcessInJob.Call(
		uintptr(process),
		uintptr(job),
		uintptr(unsafe.Pointer(&result)),
	)
	if ok == 0 {
		return false, fmt.Errorf("IsProcessInJob: %w", callErr)
	}
	return result != 0, nil
}
