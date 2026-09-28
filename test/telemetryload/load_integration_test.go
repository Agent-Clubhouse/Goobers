//go:build integration

package telemetryload_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	// testdata is excluded from ./...; explicitly keep driver invariants in CI.
	checks := exec.CommandContext(ctx, "go", "test", "./test/telemetryload/testdata/driver", "-count=1")
	checks.Dir = root
	if output, err := checks.CombinedOutput(); err != nil {
		t.Fatalf("driver invariants: %v\n%s", err, output)
	}
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
	checkStartupProbe(t, ctx, root, artifact, daemon, driver)
}

// Reuse the daemon/driver built by the existing integration fixture. One pair
// per mode checks the harness, not a release p95 distribution. The parent's
// existing three-minute context remains the bound; no timeout is relaxed.
func checkStartupProbe(t *testing.T, ctx context.Context, repo, artifact, daemon, driver string) {
	t.Helper()
	for _, index := range []string{"cold", "warm"} {
		t.Run("startup-"+index, func(t *testing.T) {
			endpoint := "healthy"
			if index == "warm" {
				endpoint = "stalled"
			}
			dir := filepath.Join(artifact, "startup-"+index)
			args := []string{"-bin", daemon, "-out", dir, "-scenario", "startup", "-startup-rounds", "1", "-startup-settle", "0s", "-startup-index", index, "-startup-endpoint", endpoint}
			if runtime.GOOS == "windows" {
				args = append(args, "-windows-insecure-demo")
			}
			command := exec.CommandContext(ctx, driver, args...)
			command.Dir = repo
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(entry), "GOOBERS_DISABLE_FSYNC=") {
					command.Env = append(command.Env, entry)
				}
			}
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("startup fixture: %v\n%s", err, output)
			}
			verifyStartupSmoke(t, dir, index)
		})
	}
}

func verifyStartupSmoke(t *testing.T, dir, index string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "startup-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Complete, OSCacheCold, FsyncOverrideAbsent bool
		Samples                                    []struct {
			Enabled, SeedPayloadsRemovedAfterward bool
			Before                                struct{ Manifest bool }
			Measurement                           struct {
				StartupMS, ShutdownMS float64
				Requests              int64
			}
			Prime json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || result.OSCacheCold || !result.FsyncOverrideAbsent || len(result.Samples) != 2 {
		t.Fatalf("invalid startup evidence: %s", data)
	}
	for i, sample := range result.Samples {
		if sample.Enabled != (i == 1) || !sample.SeedPayloadsRemovedAfterward || sample.Measurement.StartupMS <= 0 || sample.Measurement.ShutdownMS <= 0 {
			t.Fatalf("invalid startup sample: %s", data)
		}
		if sample.Before.Manifest != (index == "warm" && sample.Enabled) || (len(sample.Prime) > 0) != (index == "warm") {
			t.Fatalf("incorrect manifest/priming state: %s", data)
		}
		if sample.Enabled && sample.Measurement.Requests == 0 {
			t.Fatalf("configured endpoint was never exercised: %s", data)
		}
	}
}
