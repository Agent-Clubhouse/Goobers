package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/temporalcli"
)

const (
	temporalCLIJob      = "temporal-cli"
	temporalCacheAction = "actions/cache/restore@"
)

func TestTemporalCLIIsAcquiredOnceAndRestoredByTestJobs(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)

	if got := workflow.Env["TEMPORAL_CLI_VERSION"]; got != temporalcli.Version {
		t.Fatalf("TEMPORAL_CLI_VERSION = %q, want downloader version %q", got, temporalcli.Version)
	}
	acquire, ok := workflow.Jobs[temporalCLIJob]
	if !ok {
		t.Fatalf("CI workflow has no %q acquisition job", temporalCLIJob)
	}
	if !strings.Contains(acquire.step(t, "Download Temporal CLI").Run, "internal/temporalcli/cmd/download") {
		t.Error("Temporal CLI acquisition job must use the retrying repository downloader")
	}
	if !strings.HasPrefix(acquire.step(t, "Cache Temporal CLI").Uses, "actions/cache@") {
		t.Error("Temporal CLI acquisition must publish one pinned actions/cache entry")
	}

	for _, name := range []string{"unit", "unit-linux-coverage"} {
		job := workflow.Jobs[name]
		if !strings.Contains(strings.Join(job.Needs, " "), temporalCLIJob) {
			t.Errorf("%s does not depend on the Temporal CLI acquisition job", name)
		}
		if job.Env[temporalcli.EnvVar] == "" {
			t.Errorf("%s does not set %s to the restored binary", name, temporalcli.EnvVar)
		}
		restore := job.step(t, "Restore Temporal CLI")
		if !strings.HasPrefix(restore.Uses, temporalCacheAction) {
			t.Errorf("%s restore uses %q, want %s<pin>", name, restore.Uses, temporalCacheAction)
		}
		if !strings.Contains(restore.with("key"), "runner.os") ||
			!strings.Contains(restore.with("key"), "runner.arch") ||
			!strings.Contains(restore.with("key"), "TEMPORAL_CLI_VERSION") {
			t.Errorf("%s cache key %q must include OS, architecture, and Temporal CLI version", name, restore.with("key"))
		}
	}
}
