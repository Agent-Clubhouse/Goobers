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
		if value := job.Env[temporalcli.EnvVar]; value != "" {
			t.Errorf("%s sets job-level %s=%q; runner.* is unavailable in jobs.<id>.env, so export it from a step through GITHUB_ENV", name, temporalcli.EnvVar, value)
		}
		if job.RunsOn != "ubuntu-latest" {
			t.Errorf("%s consumes %s but runs on %q; only the Ubuntu acquisition job populates this OS/architecture cache", name, temporalcli.EnvVar, job.RunsOn)
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
		if restore.with("fail-on-cache-miss") != "true" {
			t.Errorf("%s restore must fail on a cache miss instead of exporting a nonexistent %s path", name, temporalcli.EnvVar)
		}
		export := job.step(t, "Export Temporal CLI path")
		if job.stepIndex(restore.Name) >= job.stepIndex(export.Name) {
			t.Errorf("%s must export %s after restoring the cached binary", name, temporalcli.EnvVar)
		}
		for _, want := range []string{temporalcli.EnvVar + "=$RUNNER_TEMP/temporal-cli/temporal", "$GITHUB_ENV"} {
			if !strings.Contains(export.Run, want) {
				t.Errorf("%s export step = %q, want it to contain %q", name, export.Run, want)
			}
		}
	}

	for name, job := range workflow.Jobs {
		if value := job.Env[temporalcli.EnvVar]; strings.Contains(value, "runner.") {
			t.Errorf("%s sets %s with runner context in job-level env: %q", name, temporalcli.EnvVar, value)
		}
	}
}
