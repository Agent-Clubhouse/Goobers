package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

const healthySiblingWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: healthy-sibling
spec:
  gaggle: example
  triggers:
    - type: manual
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: run a no-op local command
      run:
        command: ["true"]
`

// TestDaemonScopesHarnessPreflightFailureToDependentWorkflow proves #5163 at
// the daemon/scheduler seam: startup succeeds, the dependent workflow carries
// a visible permanent refusal, and an unrelated workflow still dispatches.
func TestDaemonScopesHarnessPreflightFailureToDependentWorkflow(t *testing.T) {
	root := initDeterministicDemo(t)
	path := filepath.Join(root, "config", "gaggles", "example", "workflows", "healthy-sibling.yaml")
	if err := os.WriteFile(path, []byte(healthySiblingWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	original := preflightHarnesses
	t.Cleanup(func() { preflightHarnesses = original })
	preflightHarnesses = func(map[string]apiv1.GooberSpec, []apiv1.Workflow, harness.EnvironmentConfig, map[string][]string, modelCredentialFor) (harnessPreflightInfo, error) {
		identity := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "default-implement"}
		return harnessPreflightInfo{}, &harnessPreflightFailures{Refusals: map[localscheduler.WorkflowIdentity]string{
			identity: `stage "implement" requires harness "copilot", whose startup preflight failed: signed out; run copilot auth login`,
		}}
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), instance.NewLayout(root), &wg)
	if err != nil {
		t.Fatalf("one failing harness must not prevent daemon setup: %v", err)
	}
	defer func() { _ = setup.Shutdown(context.Background()) }()

	var dependent, healthy *localscheduler.WorkflowEntry
	for i := range setup.Entries {
		switch setup.Entries[i].Workflow {
		case "default-implement":
			dependent = &setup.Entries[i]
		case "healthy-sibling":
			healthy = &setup.Entries[i]
		}
	}
	if dependent == nil || healthy == nil {
		t.Fatalf("expected dependent and healthy entries: %+v", setup.Entries)
	}
	if !strings.Contains(dependent.HarnessRefusal, "signed out") {
		t.Fatalf("dependent workflow has no actionable harness refusal: %+v", dependent)
	}
	if healthy.HarnessRefusal != "" {
		t.Fatalf("unrelated workflow inherited harness refusal: %q", healthy.HarnessRefusal)
	}

	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	_, err = sched.Trigger(context.Background(), "default-implement", time.Now())
	var rejected *localscheduler.TriggerRejectedError
	if !errors.As(err, &rejected) || !strings.HasPrefix(rejected.Reason, localscheduler.ReasonHarnessUnavailable) {
		t.Fatalf("dependent trigger = %v, want visible %q refusal", err, localscheduler.ReasonHarnessUnavailable)
	}
	if _, err := sched.Trigger(context.Background(), "healthy-sibling", time.Now()); err != nil {
		t.Fatalf("unrelated workflow must still dispatch: %v", err)
	}
	wg.Wait()

	events, err := journal.ReadInstanceLog(setup.InstanceLog.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var sawDependentRefusal bool
	for _, event := range events {
		if event.Type == journal.EventWorkflowRefused && event.Workflow == "default-implement" && strings.Contains(event.Reason, "signed out") {
			sawDependentRefusal = true
		}
		if event.Type == journal.EventWorkflowRefused && event.Workflow == "healthy-sibling" {
			t.Fatalf("healthy sibling must not be journaled refused: %+v", event)
		}
	}
	if !sawDependentRefusal {
		t.Fatalf("workflow.refused did not expose the harness failure: %+v", events)
	}
}
