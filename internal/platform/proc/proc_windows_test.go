//go:build windows

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

	"golang.org/x/sys/windows"
)

func TestStartAttachesBeforeChildExecutes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command(
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		"Set-Content -LiteralPath $env:GOOBERS_PROCESS_MARKER -Value started; Start-Sleep -Seconds 30",
	)
	cmd.Env = append(os.Environ(), "GOOBERS_PROCESS_MARKER="+marker)
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

	// 25s, not the child's full 30s budget: generous enough to absorb a cold
	// powershell.exe start under real Windows CI contention (the observed
	// merge_group flake, #2048 — a fixed 5s deadline for spawning and
	// dispatching a real external process was too tight for a loaded shared
	// runner, not evidence of a broken attach/resume path) while still
	// leaving margin below the child's Start-Sleep window.
	deadline := time.Now().Add(25 * time.Second)
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

func TestKillTerminatesPowerShellDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command(
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		"$child = Start-Process powershell.exe -ArgumentList '-NoLogo','-NoProfile','-NonInteractive','-Command','$grandchild = Start-Process powershell.exe -ArgumentList ''-NoLogo'',''-NoProfile'',''-NonInteractive'',''-Command'',''Start-Sleep -Seconds 30'' -PassThru; Set-Content -LiteralPath $env:GOOBERS_GRANDCHILD_PID -Value $grandchild.Id; Start-Sleep -Seconds 30' -PassThru; Set-Content -LiteralPath $env:GOOBERS_CHILD_PID -Value $child.Id; Start-Sleep -Seconds 30",
	)
	grandchildMarker := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd.Env = append(os.Environ(), "GOOBERS_CHILD_PID="+marker, "GOOBERS_GRANDCHILD_PID="+grandchildMarker)
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		_ = tree.Kill()
		_ = cmd.Wait()
	}()

	var childPID int
	deadline := time.Now().Add(5 * time.Second)
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
	deadline = time.Now().Add(5 * time.Second)
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

func TestProcessTreeHelper(t *testing.T) {
	role := os.Getenv("GOOBERS_PROC_HELPER_ROLE")
	if role == "" {
		return
	}
	pidMarker := os.Getenv("GOOBERS_PROC_HELPER_PID")
	grandchildMarker := os.Getenv("GOOBERS_PROC_HELPER_GRANDCHILD")
	if role == "root" {
		cmd := exec.Command(os.Args[0], "-test.run=TestProcessTreeHelper")
		cmd.Env = append(os.Environ(), "GOOBERS_PROC_HELPER_ROLE=child")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pidMarker, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
	} else if role == "child" {
		cmd := exec.Command(os.Args[0], "-test.run=TestProcessTreeHelper")
		cmd.Env = append(os.Environ(), "GOOBERS_PROC_HELPER_ROLE=grandchild")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(grandchildMarker, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(30 * time.Second)
}

func TestKillTerminatesEscapedDescendants(t *testing.T) {
	childMarker := filepath.Join(t.TempDir(), "child.pid")
	grandchildMarker := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := exec.Command(os.Args[0], "-test.run=TestProcessTreeHelper")
	cmd.Env = append(os.Environ(),
		"GOOBERS_PROC_HELPER_ROLE=root",
		"GOOBERS_PROC_HELPER_PID="+childMarker,
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
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		t.Skip("wsl.exe is not installed")
	}

	marker := filepath.Join(t.TempDir(), "wsl.pid")
	cmd := exec.Command(
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		"$child = Start-Process wsl.exe -ArgumentList '-e','sh','-c','sleep 30' -PassThru; Set-Content -LiteralPath $env:GOOBERS_WSL_PID -Value $child.Id; Start-Sleep -Seconds 30",
	)
	cmd.Env = append(os.Environ(), "GOOBERS_WSL_PID="+marker)
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tree.Kill()
		_ = cmd.Wait()
	}()

	var wslPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(marker)
		if readErr == nil {
			wslPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if wslPID == 0 {
		t.Fatal("WSL process did not record its pid")
	}
	if !Alive(wslPID) {
		t.Fatalf("WSL process %d exited before tree termination", wslPID)
	}
	guestDescendants, snapshotErr := snapshotDescendants(wslPID)
	if snapshotErr != nil {
		t.Fatalf("snapshot WSL descendants: %v", snapshotErr)
	}
	if len(guestDescendants) == 0 {
		t.Fatal("WSL launcher did not retain a host or guest descendant")
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
	for _, descendant := range guestDescendants {
		if Alive(descendant.pid) {
			t.Fatalf("WSL descendant %d survived tree termination", descendant.pid)
		}
	}
}
