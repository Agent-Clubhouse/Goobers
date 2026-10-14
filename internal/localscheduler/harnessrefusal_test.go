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

// fakeHarnessRecheck reports a scripted refusal and the context it was
// consulted under.
type fakeHarnessRecheck struct {
	mu      sync.Mutex
	refusal string
	ctx     context.Context
}

func (f *fakeHarnessRecheck) HarnessRefusal(ctx context.Context, _ time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctx = ctx
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
// workflow dispatches without a restart or configuration change. Every change
// in the refusal reaches the instance journal, so status reflects it.
func TestTransientHarnessRefusalClearsWhenRecheckRecovers(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	transient := `stage "implement" requires harness "copilot", whose startup preflight failed: API rate limit exceeded (transient provider failure; next probe after 2026-10-14T12:00:00Z)`
	recheck := &fakeHarnessRecheck{refusal: transient}
	entry := WorkflowEntry{
		Workflow:       "needs-copilot",
		Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:        starter,
		HarnessRefusal: transient,
		HarnessRecheck: recheck,
	}
	sched, dir := newTestScheduler(t, []WorkflowEntry{entry})

	_, err := sched.Trigger(context.Background(), "needs-copilot", time.Now())
	var rejected *TriggerRejectedError
	if !errors.As(err, &rejected) || !strings.HasPrefix(rejected.Reason, ReasonHarnessUnavailable) || !strings.Contains(rejected.Reason, "transient provider failure") {
		t.Fatalf("trigger while the provider is rate limited = %v, want a harness-unavailable refusal naming the transient cause", err)
	}
	if starter.count() != 0 {
		t.Fatalf("refused workflow started %d times", starter.count())
	}
	if !sched.skipRefusedEntry(entry, time.Now()) {
		t.Fatal("a still-refused transient entry must be skipped by the tick like a permanent one")
	}

	// The next probe time moved: journaled once as a fresh refusal.
	later := strings.Replace(transient, "12:00:00Z", "12:02:00Z", 1)
	recheck.set(later)
	sched.skipRefusedEntry(entry, time.Now())
	sched.skipRefusedEntry(entry, time.Now())

	recheck.set("")
	if sched.skipRefusedEntry(entry, time.Now()) {
		t.Fatal("a recovered transient entry is still skipped")
	}
	if _, err := sched.Trigger(context.Background(), "needs-copilot", time.Now()); err != nil {
		t.Fatalf("trigger after the harness recovered: %v", err)
	}
	waitForCount(t, func() int { return starter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var transitions []string
	for _, event := range events {
		if event.Workflow != "needs-copilot" {
			continue
		}
		switch event.Type {
		case journal.EventWorkflowRefused:
			transitions = append(transitions, "refused: "+event.Reason)
		case journal.EventWorkflowReadmitted:
			transitions = append(transitions, "readmitted")
		}
	}
	want := []string{"refused: " + transient, "refused: " + later, "readmitted"}
	if strings.Join(transitions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("journaled transitions =\n%s\nwant\n%s", strings.Join(transitions, "\n"), strings.Join(want, "\n"))
	}
}

// TestHarnessRecheckRunsUnderSchedulerLifetime: a recheck's background
// re-probe is bound to the scheduler's own lifetime, so stopping the daemon
// stops it.
func TestHarnessRecheckRunsUnderSchedulerLifetime(t *testing.T) {
	recheck := &fakeHarnessRecheck{refusal: "transient"}
	entry := WorkflowEntry{Workflow: "needs-copilot", HarnessRefusal: "transient", HarnessRecheck: recheck}
	sched, _ := newTestScheduler(t, []WorkflowEntry{entry})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sched.harnessRechecks.setLifetime(ctx)
	sched.skipRefusedEntry(entry, time.Now())
	recheck.mu.Lock()
	defer recheck.mu.Unlock()
	if recheck.ctx == nil || recheck.ctx.Err() == nil {
		t.Fatal("the recheck was not consulted under the scheduler's lifetime context")
	}
}

func TestHarnessRefusalWithoutRecheckStaysPermanent(t *testing.T) {
	sched, _ := newTestScheduler(t, nil)
	if !sched.skipRefusedEntry(WorkflowEntry{HarnessRefusal: "signed out"}, time.Now()) {
		t.Fatal("a harness refusal without a recheck must stay permanent")
	}
	if sched.skipRefusedEntry(WorkflowEntry{}, time.Now()) {
		t.Fatal("an unrefused entry was skipped")
	}
}
