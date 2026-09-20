package localscheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestHarnessRefusalRejectsOnlyDependentWorkflow(t *testing.T) {
	refusedStarter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	healthyStarter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{
			Workflow:       "needs-copilot",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:        refusedStarter,
			HarnessRefusal: `stage "implement" requires harness "copilot": signed out`,
		},
		{
			Workflow:  "healthy",
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:   healthyStarter,
		},
	})

	_, err := sched.Trigger(context.Background(), "needs-copilot", time.Now())
	var rejected *TriggerRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("trigger error = %T %v, want *TriggerRejectedError", err, err)
	}
	if !strings.HasPrefix(rejected.Reason, ReasonHarnessUnavailable) || !strings.Contains(rejected.Reason, "signed out") {
		t.Fatalf("trigger rejection is not actionable: %q", rejected.Reason)
	}
	if rejected.Transient() {
		t.Fatal("harness refusal must remain fail-closed until config/startup refresh")
	}
	if refusedStarter.count() != 0 {
		t.Fatalf("refused workflow started %d times", refusedStarter.count())
	}

	if _, err := sched.Trigger(context.Background(), "healthy", time.Now()); err != nil {
		t.Fatalf("healthy sibling trigger: %v", err)
	}
	waitForCount(t, func() int { return healthyStarter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawRefusal bool
	for _, event := range events {
		if event.Type == journal.EventWorkflowRefused && event.Workflow == "needs-copilot" && strings.Contains(event.Reason, "signed out") {
			sawRefusal = true
		}
		if event.Type == journal.EventWorkflowRefused && event.Workflow == "healthy" {
			t.Fatalf("healthy sibling was journaled refused: %+v", event)
		}
	}
	if !sawRefusal {
		t.Fatalf("missing visible workflow.refused event: %+v", events)
	}
}
