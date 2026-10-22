package main

import (
	"slices"
	"testing"
)

func TestCIBranchCoverage(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	for _, event := range []string{"push", "pull_request", "merge_group"} {
		t.Run(event, func(t *testing.T) {
			var trigger struct {
				Branches []string `yaml:"branches"`
				Types    []string `yaml:"types"`
			}
			node, ok := workflow.On[event]
			if !ok {
				t.Fatalf("CI lacks %s trigger", event)
			}
			if err := node.Decode(&trigger); err != nil {
				t.Fatalf("decode %s trigger: %v", event, err)
			}
			if !slices.Equal(trigger.Branches, []string{"main", "release/**"}) {
				t.Errorf("%s branches = %v, want main and release/** (including nested release branches)", event, trigger.Branches)
			}
			if event == "pull_request" && !slices.Equal(trigger.Types, []string{"opened", "synchronize", "reopened"}) {
				t.Errorf("pull_request types = %v, want opened, synchronize, reopened without metadata-only edits", trigger.Types)
			}
		})
	}
}

func TestCIMainOnlyMaintenance(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	for _, id := range []string{declaredCacheWarmJob, raceCacheWarmJob, "escalate-main-failure"} {
		want := "${{ github.event_name == 'push' && github.ref == 'refs/heads/main' }}"
		if id == "escalate-main-failure" {
			want = "${{ always() && github.event_name == 'push' && github.ref == 'refs/heads/main' }}"
		}
		if got := workflow.Jobs[id].If; got != want {
			t.Errorf("%s condition = %q, want %q so release pushes cannot warm default-branch caches or change main's incident", id, got, want)
		}
	}
}

func TestCIReleaseValidationPreservesGate(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	aggregate := workflow.Jobs["required-ci"]
	const requiredName = "make ci (fmt-check · vet · build · test · lint)"
	if aggregate.Name != requiredName {
		t.Errorf("required CI name = %q, want %q", aggregate.Name, requiredName)
	}
	if aggregate.If != "${{ always() && github.event_name != 'push' }}" || aggregate.ContinueOnError || len(aggregate.Needs) == 0 {
		t.Fatal("pull requests and merge groups must retain the full fail-closed aggregate")
	}
	for _, id := range aggregate.Needs {
		job, ok := workflow.Jobs[id]
		if !ok {
			t.Fatalf("required job %s is undefined", id)
		}
		if job.If != "" && job.If != "${{ github.event_name != 'push' }}" {
			t.Errorf("required job %s has a condition that can skip release pull requests or merge groups: %s", id, job.If)
		}
		if job.ContinueOnError {
			t.Errorf("required job %s must remain fail-closed", id)
		}
		checkout := job.stepUsing(t, "actions/checkout@")
		if ref := checkout.with("ref"); ref != "" && ref != "${{ github.sha }}" {
			t.Errorf("required job %s must check out its event SHA, not %q", id, ref)
		}
	}
	for _, id := range []string{"preflight", "checks", "deploy-reference", "lint", "darwin-build", "unit", "unit-linux-coverage", "shipped", "windows-smoke"} {
		job, ok := workflow.Jobs[id]
		if !ok || job.If != "" || job.ContinueOnError {
			t.Errorf("landed-commit validation job %s must run on both main and release pushes and remain fail-closed", id)
		}
	}
}
