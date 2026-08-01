package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// TestUpPrunesAbandonedRunCreationStagingDirAtStartup is #2035's acceptance
// test: a crash-abandoned run-creation staging directory left before this
// daemon process started must be removed as part of startup housekeeping —
// the same precedent already established for worktree.Reap
// (TestUpReapsTerminalDeregisteredOrphanAndKeepsMarkedWorktree) and telemetry
// retention. A directory shaped like an in-flight creation (fresh, well
// under the 24h floor) must survive the same startup pass.
func TestUpPrunesAbandonedRunCreationStagingDirAtStartup(t *testing.T) {
	root := initDeterministicDemo(t)
	setAPIListenAddress(t, root, freeLoopbackAddress(t))
	l := instance.NewLayout(root)
	runsDir := l.ForGaggle("example").RunsDir()

	stagingRoot := journal.RunCreationStagingDir(runsDir)
	abandoned := filepath.Join(stagingRoot, "abandoned-run-123456789")
	inFlight := filepath.Join(stagingRoot, "inflight-run-123456789")
	for _, dir := range []string{abandoned, inFlight} {
		if err := os.MkdirAll(filepath.Join(dir, "spans"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Older than the 24h safety floor: a crash left this behind well before
	// this process ever started.
	setOrphanFixtureModTime(t, abandoned, time.Now().Add(-25*time.Hour))
	// inFlight keeps its natural just-created mtime — the shape of a creation
	// genuinely in progress when the daemon started, which must never be swept
	// regardless of what triggers a sweep.

	ctx, cancel := context.WithCancel(context.Background())
	stdout := newDaemonOutput()
	var stderr bytes.Buffer
	daemonDone := make(chan int, 1)
	go func() { daemonDone <- runUpContext(ctx, []string{root}, stdout, &stderr) }()
	daemonStopped := false
	t.Cleanup(func() {
		if daemonStopped {
			return
		}
		cancel()
		select {
		case <-daemonDone:
		case <-time.After(10 * time.Second):
			t.Error("runUpContext did not stop during cleanup")
		}
	})

	select {
	case <-stdout.started:
	case code := <-daemonDone:
		daemonStopped = true
		t.Fatalf("runUpContext exited before startup: code=%d stderr=%q", code, stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("runUpContext did not report daemon readiness")
	}
	cancel()
	select {
	case code := <-daemonDone:
		daemonStopped = true
		if code != 0 {
			t.Fatalf("runUpContext: code=%d stderr=%q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not stop after cancellation")
	}

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned run-creation staging dir still exists after startup: %v", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("in-flight-shaped staging dir was removed at startup: %v", err)
	}
	if !strings.Contains(stdout.String(), "pruned orphan run directory") {
		t.Errorf("stdout = %q, want a pruned-orphan startup message", stdout.String())
	}
}

func setOrphanFixtureModTime(t *testing.T, root string, at time.Time) {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err == nil {
			paths = append(paths, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for index := len(paths) - 1; index >= 0; index-- {
		if err := os.Chtimes(paths[index], at, at); err != nil {
			t.Fatal(err)
		}
	}
}
