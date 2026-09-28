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
)

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
