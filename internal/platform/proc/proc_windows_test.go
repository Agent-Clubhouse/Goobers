//go:build windows

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
		"$child = Start-Process powershell.exe -ArgumentList '-NoLogo','-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 30' -PassThru; Set-Content -LiteralPath $env:GOOBERS_CHILD_PID -Value $child.Id; Start-Sleep -Seconds 30",
	)
	cmd.Env = append(os.Environ(), "GOOBERS_CHILD_PID="+marker)
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
}
