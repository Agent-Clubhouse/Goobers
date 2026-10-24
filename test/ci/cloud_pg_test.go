package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCloudPGJobIsPathConditionalAndPinned(t *testing.T) {
	workflow := loadCIWorkflow(t)
	job := workflow.Jobs["cloud-pg"]
	if job.Name != "cloud PostgreSQL and Azurite integration" ||
		job.TimeoutMinutes != 30 ||
		!slices.Contains(job.Needs, "scope") ||
		!slices.Contains(job.Needs, "temporal-cli") ||
		!strings.Contains(job.If, "needs.scope.outputs.cloud == 'true'") {
		t.Fatalf("cloud-pg job is not the bounded path-conditional gate: %+v", job)
	}
	if job.Services["postgres"].Image != "postgres:16@sha256:a902560563d3a5661228585f53474c5065e7f273b21ffd77e555a8b5682c5690" {
		t.Fatalf("postgres service = %+v", job.Services["postgres"])
	}
	azurite := job.Services["azurite"]
	if azurite.Image != "mcr.microsoft.com/azure-storage/azurite:3.37.0@sha256:830430c1da1a2d537e08f3e6764dd1f5ae00cf0346bcaf625b968ec3f0971fd5" ||
		azurite.Env["AZURITE_ACCOUNTS"] == "" {
		t.Fatalf("azurite service = %+v", azurite)
	}
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	section := workflowJob(string(data), "cloud-pg")
	for _, pin := range []string{
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		"actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
		"actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9",
	} {
		if !strings.Contains(section, pin) {
			t.Errorf("cloud-pg omits pinned action %s", pin)
		}
	}
	order := job.step(t, "Enforce migration numbering above the base")
	// The gate compares against the base, not the merge-base, so a stale PR
	// fails on update-branch until it renumbers.
	if order.Env["BASE_REF"] != "${{ github.event.pull_request.base.sha || github.event.merge_group.base_sha || inputs.base_sha || 'HEAD' }}" ||
		!strings.Contains(order.Run, `git fetch --no-tags --depth=1 origin "$BASE_REF"`) ||
		!strings.Contains(order.Run, `go run ./test/migrationorder -base-ref "$BASE_REF" -head-ref HEAD`) {
		t.Fatalf("migration numbering gate = %+v", order)
	}
	run := job.step(t, "Run cloud integration smoke")
	// -p 1: the packages share cluster-wide PostgreSQL roles. The -json stream
	// must reach cloud-pg-report, which fails the job when a cloudintegration
	// package ran no passing test, and it is uploaded as an artifact.
	if !strings.Contains(run.Run, "go test -p 1 ") || !strings.Contains(run.Run, "-tags=integration,cloudpg,cloudintegration") ||
		!strings.Contains(run.Run, " -json ") || !strings.Contains(run.Run, "set -uo pipefail") ||
		!strings.Contains(run.Run, `tee "$RUNNER_TEMP/cloud-pg-test.json"`) ||
		!strings.Contains(run.Run, `go run ./test/ci cloud-pg-report "$RUNNER_TEMP/cloud-pg-test.json" || status=1`) ||
		!strings.Contains(run.Run, "./internal/cloud/... ./internal/livejournal/... ./test/ha/invariants/... ./test/journalequivalence/... ./cmd/goobers-cloud") ||
		run.Env["TESTDEP_STRICT"] != "1" {
		t.Fatalf("cloud integration step = %+v", run)
	}
	pending := job.step(t, "Verify journal equivalence pending issues")
	if pending.Run != "go run ./test/journalequivalence/cmd/equivalence verify-pending" || pending.Env["GH_TOKEN"] != "${{ github.token }}" || pending.ContinueOnError {
		t.Fatalf("known journal divergences are not checked live: %+v", pending)
	}
	upload := job.step(t, "Upload cloud integration test events")
	if upload.If != "always()" || !strings.Contains(upload.Uses, "actions/upload-artifact@") ||
		upload.With["path"] != "${{ runner.temp }}/cloud-pg-test.json" {
		t.Fatalf("cloud integration events are not always uploaded: %+v", upload)
	}
	journal := job.step(t, "Run PostgreSQL journal conformance")
	if !strings.Contains(journal.Run, "go test -race -tags=cloudintegration") ||
		!strings.Contains(journal.Run, "./internal/journal") ||
		!strings.Contains(journal.Run, "Journal.*Postgres") ||
		journal.Env["GOOBERS_TEST_POSTGRES_URL"] == "" {
		t.Fatalf("journal conformance is not gated against PG16: %+v", journal)
	}
	restart := job.step(t, "Restart Azurite with telemetry disabled")
	if !strings.Contains(restart.Run, "--disableTelemetry --skipApiVersionCheck") ||
		!strings.Contains(restart.Run, azurite.Image) {
		t.Fatalf("Azurite restart does not enforce reviewed flags: %+v", restart)
	}
	if !slices.Contains(workflow.Jobs["required-ci"].Needs, "cloud-pg") {
		t.Fatal("required-ci does not include cloud-pg")
	}
}

func TestCloudPGTimingIsAdvisoryAndIncludesSetup(t *testing.T) {
	job := loadCIWorkflow(t).Jobs["cloud-pg"]
	report := job.step(t, "Report cloud job timing budget")
	if report.If != "always()" || !report.ContinueOnError ||
		!strings.Contains(report.Run, "/attempts/$GITHUB_RUN_ATTEMPT/jobs") ||
		!strings.Contains(report.Run, ".started_at") ||
		!strings.Contains(report.Run, "testtiming wall -job cloud-pg") {
		t.Fatalf("cloud job wall timing must include setup and remain advisory: %+v", report)
	}
	upload := job.step(t, "Upload cloud job timing")
	if upload.If != "always()" || !upload.ContinueOnError || upload.with("path") != "test-timings/cloud-pg.json" {
		t.Fatalf("cloud timing evidence not retained: %+v", upload)
	}
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "test-timing-budgets.json"))
	if err != nil {
		t.Fatal(err)
	}
	var budgets struct {
		Jobs map[string]struct {
			BudgetSeconds int  `json:"budgetSeconds"`
			AlertOnBudget bool `json:"alertOnBudget"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(data, &budgets); err != nil {
		t.Fatal(err)
	}
	budget := budgets.Jobs["cloud-pg"]
	if budget.BudgetSeconds != 900 || !budget.AlertOnBudget || job.TimeoutMinutes*60 <= budget.BudgetSeconds {
		t.Fatalf("cloud job needs a soft 15m budget and separate hang guard: %+v", budget)
	}
}
