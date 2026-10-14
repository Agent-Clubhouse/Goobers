package localscheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
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

// fakeHarnessRecheck reports a scripted refusal.
type fakeHarnessRecheck struct {
	mu      sync.Mutex
	refusal string
}

func (f *fakeHarnessRecheck) HarnessRefusal(time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refusal
}

func (f *fakeHarnessRecheck) set(refusal string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusal = refusal
}

// TestTransientHarnessRefusalClearsWhenRecheckRecovers is #6017: a startup
// harness refusal caused by a rate-limited provider is not permanent for the
// configuration. While the recheck still reports it the workflow is refused
// exactly as before; once the recheck reports the harness usable, the same
// workflow dispatches without a restart or configuration change.
func TestTransientHarnessRefusalClearsWhenRecheckRecovers(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	transient := `stage "implement" requires harness "copilot", whose startup preflight failed: API rate limit exceeded (transient provider failure; next probe after 2026-10-14T12:00:00Z)`
	recheck := &fakeHarnessRecheck{refusal: transient}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:       "needs-copilot",
		Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:        starter,
		HarnessRefusal: transient,
		HarnessRecheck: recheck,
	}})

	_, err := sched.Trigger(context.Background(), "needs-copilot", time.Now())
	var rejected *TriggerRejectedError
	if !errors.As(err, &rejected) || !strings.HasPrefix(rejected.Reason, ReasonHarnessUnavailable) || !strings.Contains(rejected.Reason, "transient provider failure") {
		t.Fatalf("trigger while the provider is rate limited = %v, want a harness-unavailable refusal naming the transient cause", err)
	}
	if starter.count() != 0 {
		t.Fatalf("refused workflow started %d times", starter.count())
	}
	entry := WorkflowEntry{HarnessRefusal: transient, HarnessRecheck: recheck}
	if !skipRefusedEntry(entry, time.Now()) {
		t.Fatal("a still-refused transient entry must be skipped by the tick like a permanent one")
	}

	recheck.set("")
	if skipRefusedEntry(entry, time.Now()) {
		t.Fatal("a recovered transient entry is still skipped")
	}
	if _, err := sched.Trigger(context.Background(), "needs-copilot", time.Now()); err != nil {
		t.Fatalf("trigger after the harness recovered: %v", err)
	}
	waitForCount(t, func() int { return starter.count() }, 1)
}

func TestHarnessRefusalWithoutRecheckStaysPermanent(t *testing.T) {
	if !skipRefusedEntry(WorkflowEntry{HarnessRefusal: "signed out"}, time.Now()) {
		t.Fatal("a harness refusal without a recheck must stay permanent")
	}
	if skipRefusedEntry(WorkflowEntry{}, time.Now()) {
		t.Fatal("an unrefused entry was skipped")
	}
}
