//go:build windows

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
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
	time.Sleep(100 * time.Millisecond)
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
		time.Sleep(10 * time.Millisecond)
	}
}
