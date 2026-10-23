package main

import (
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
		job.TimeoutMinutes != 15 ||
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
	run := job.step(t, "Run cloud integration smoke")
	// -p 1: the packages share cluster-wide PostgreSQL roles.
	if !strings.Contains(run.Run, "go test -p 1 ") || !strings.Contains(run.Run, "-tags=integration,cloudpg,cloudintegration") ||
		!strings.Contains(run.Run, "./internal/cloud/...") ||
		run.Env["TESTDEP_STRICT"] != "1" {
		t.Fatalf("cloud integration step = %+v", run)
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
