//go:build windows

package proc

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestOpenIdentityForTerminatePinsRecordedProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(), "GOOBERS_PROC_HELPER_ROLE=short")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	started, ok := startTime(cmd.Process.Pid)
	if !ok {
		t.Fatal("helper process start time was not readable")
	}
	if target, err := openIdentityForTerminate(processIdentity{
		pid:       cmd.Process.Pid,
		startTime: started.Add(time.Nanosecond),
	}); err != nil || target.handle != 0 {
		target.close()
		t.Fatalf("open mismatched identity = (handle=%v, err=%v), want no target", target.handle, err)
	}

	target, err := openIdentityForTerminate(processIdentity{pid: cmd.Process.Pid, startTime: started})
	if err != nil {
		t.Fatalf("open recorded identity: %v", err)
	}
	if target.handle == 0 {
		t.Fatal("open recorded identity returned no target")
	}
	defer target.close()
	if err := target.terminate(); err != nil {
		t.Fatalf("terminate pinned identity: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper unexpectedly exited successfully after termination")
	}
	if got := identityStateForHandle(target.handle, started); got != identityGone {
		t.Fatalf("pinned identity after termination = %v, want gone", got)
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
	got := identifyDescendantsWithStartTime(10, map[int][]int{
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
	_, _ = os.Stderr.WriteString("WSL helper: starting wsl.exe\n")
	cmd := exec.Command("wsl.exe", "-e", "sh", "-c", "sleep 90")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start WSL launcher: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	_, _ = os.Stderr.WriteString("WSL helper: started launcher PID " + strconv.Itoa(cmd.Process.Pid) + "\n")
	if err := os.WriteFile(marker, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatalf("record WSL launcher PID: %v", err)
	}
	_, _ = os.Stderr.WriteString("WSL helper: recorded launcher PID\n")
	// An early launcher exit is a setup failure, even if its exit code is zero.
	// The parent must observe a live WSL subtree before testing termination.
	err := cmd.Wait()
	t.Fatalf("WSL launcher exited before tree termination: %v", err)
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

	if err := tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
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
	deadline := time.Now().Add(10 * time.Second)
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
		t.Fatalf("WSL process did not record its pid within 10s: launcher=%d alive=%t, marker=%q, error=%v", cmd.Process.Pid, Alive(cmd.Process.Pid), markerData, markerErr)
	}
	if !Alive(wslPID) {
		t.Fatalf("WSL process %d exited before tree termination", wslPID)
	}
	// Starting wsl.exe and its brokered descendants are asynchronous. Wait for
	// both within the original readiness deadline; do not rely on incidental
	// PowerShell/marker overhead to give the WSL broker time to start.
	var guestDescendants []processIdentity
	for time.Now().Before(deadline) {
		var snapshotErr error
		guestDescendants, snapshotErr = snapshotDescendants(wslPID)
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

	// Terminating the job alone must not be enough: WSL can broker a host
	// descendant outside the job, which Tree.Kill must clean up separately.
	if err := windows.TerminateJobObject(tree.job, 1); err != nil {
		t.Fatalf("terminate WSL job: %v", err)
	}
	var brokeredDescendant processIdentity
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, descendant := range guestDescendants {
			if identityStateForPID(descendant.pid, descendant.startTime) == identityStatePresent {
				brokeredDescendant = descendant
				break
			}
		}
		if brokeredDescendant.pid != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if brokeredDescendant.pid == 0 {
		t.Fatal("WSL host descendant did not survive job termination")
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
	if identityStateForPID(brokeredDescendant.pid, brokeredDescendant.startTime) == identityStatePresent {
		t.Fatalf("WSL host descendant %d survived tree termination", brokeredDescendant.pid)
	}
}
