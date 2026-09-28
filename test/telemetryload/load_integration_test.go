//go:build integration

package telemetryload_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// The short CI fixture exercises actual daemon startup, multiple workflows,
// persisted journals, export, reconciliation and graceful shutdown. Long soaks
// and external Azure traffic are explicit release operations, never this test.
func TestIntegrationTelemetryDaemonReconcilesJournal(t *testing.T) {
	testdep.Require(t, "go", "git")
	if runtime.GOOS == "windows" {
		testdep.Require(t, "powershell.exe")
	} else {
		testdep.Require(t, "ps")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	artifact := t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	daemon, driver := filepath.Join(artifact, "goobers"+ext), filepath.Join(artifact, "load"+ext)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	for _, build := range []struct{ output, pkg string }{{daemon, "./cmd/goobers"}, {driver, "./test/telemetryload/testdata/driver"}} {
		command := exec.CommandContext(ctx, "go", "build", "-o", build.output, build.pkg)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, output)
		}
	}
	outputRoot := filepath.Join(artifact, "results")
	args := []string{"-bin", daemon, "-out", outputRoot, "-scenario", "enabled", "-duration", "15s", "-workers", "2"}
	if runtime.GOOS == "windows" {
		args = append(args, "-windows-insecure-demo")
	}
	command := exec.CommandContext(ctx, driver, args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("load fixture: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(outputRoot, "enabled.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Runs, Failures, HealthFailures, ExpectedRunEvents, MissingRunEvents, MetricSampleErrors int
		ShutdownMS                                                                              float64
		Replay                                                                                  struct{ AccountingReady bool }
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Replay.AccountingReady {
		t.Fatal("enabled fixture completed without usable replay accounting")
	}
	if result.Runs < 2 || result.Failures != 0 || result.HealthFailures != 0 || result.ExpectedRunEvents == 0 || result.MissingRunEvents != 0 || result.MetricSampleErrors != 0 || result.ShutdownMS > 20000 {
		t.Fatalf("release invariant failed: %s\n%s", data, output)
	}
	t.Logf("reconciled %d journal events across %d workflows", result.ExpectedRunEvents, result.Runs)
}
