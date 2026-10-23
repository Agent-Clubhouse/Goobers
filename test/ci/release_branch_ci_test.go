package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCIReleaseBranchTriggers(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	for _, event := range []string{"pull_request", "merge_group", "push"} {
		var trigger struct {
			Branches []string `yaml:"branches"`
			Types    []string `yaml:"types"`
			Paths    []string `yaml:"paths"`
			Ignore   []string `yaml:"paths-ignore"`
		}
		node, ok := w.On[event]
		if !ok {
			t.Fatalf("missing %s trigger", event)
		}
		if err := node.Decode(&trigger); err != nil {
			t.Fatal(err)
		}
		want := []string{"main", "release/**"}
		if event == "push" {
			want = []string{"main"}
		}
		if !slices.Equal(trigger.Branches, want) || len(trigger.Paths)+len(trigger.Ignore) != 0 {
			t.Errorf("%s must run without path filters on %v: %+v", event, want, trigger)
		}
		if event == "pull_request" && !slices.Equal(trigger.Types, []string{"opened", "synchronize", "reopened"}) {
			t.Errorf("PR events must exclude metadata-only edits: %v", trigger.Types)
		}
	}
}

func TestCIValidationCancellationIsJobScoped(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	if w.Concurrency.Group != "" || w.Concurrency.CancelInProgress {
		t.Fatal("workflow-wide cancellation can suppress the required report")
	}
	suffix := map[string]string{
		"lint": "-${{ matrix.goos }}", "unit": "-${{ matrix.shard }}", "shipped": "-${{ matrix.os }}",
	}
	for id, job := range w.Jobs {
		if id == "required-ci" || id == "escalate-main-failure" || strings.HasPrefix(id, "cancel-on-") {
			if job.Concurrency.Group != "" || job.Concurrency.CancelInProgress {
				t.Errorf("reporter/canceller %s must not occupy a cancelling concurrency group", id)
			}
			continue
		}
		want := "ci-${{ github.workflow }}-${{ github.ref }}-" + id + suffix[id]
		if job.Concurrency.Group != want || !job.Concurrency.CancelInProgress {
			t.Errorf("%s concurrency = %+v, want %s with cancellation", id, job.Concurrency, want)
		}
	}
}

func TestCIReleasePRPreservesFullMergeGate(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	aggregate := w.Jobs["required-ci"]
	if aggregate.If != "${{ always() && github.event_name != 'push' }}" || aggregate.ContinueOnError {
		t.Fatal("release PRs and merge groups must retain the unconditional aggregate")
	}
	for _, id := range aggregate.Needs {
		job := w.Jobs[id]
		if job.If != "" && job.If != "${{ github.event_name != 'push' }}" {
			t.Errorf("required job %s may skip a release PR: %s", id, job.If)
		}
		if job.ContinueOnError {
			t.Errorf("required job %s tolerates failure", id)
		}
		checkout := job.stepUsing(t, "actions/checkout@")
		if checkout.with("ref") != "" || checkout.with("repository") != "" {
			t.Errorf("%s must validate the event merge, not another ref/repository", id)
		}
	}
	gate := aggregate.step(t, "Verify required gates")
	if gate.If != "always()" || gate.ContinueOnError ||
		!strings.Contains(gate.Run, `if [ "$2" != "success" ]; then`) {
		t.Fatal("required gate must reject every non-success dependency")
	}
	veto := aggregate.step(t, "Reject cancelled validation")
	if veto.If != "cancelled()" || veto.ContinueOnError || !strings.Contains(veto.Run, "exit 1") ||
		aggregate.Steps[len(aggregate.Steps)-1].Name != veto.Name {
		t.Fatal("the final cancellation veto must fail even after dependencies succeed")
	}
}
