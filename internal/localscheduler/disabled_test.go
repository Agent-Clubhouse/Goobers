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

func TestDisabledReasonRefusesDispatchAndOthersServe(t *testing.T) {
	disabledStarter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	healthyStarter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{
			Workflow:       "paused",
			Gaggle:         "example",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:        disabledStarter,
			DisabledReason: `workflow "paused" is disabled (spec.enabled=false)`,
		},
		{
			Workflow:  "healthy",
			Gaggle:    "example",
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:   healthyStarter,
		},
	})

	_, err := sched.Trigger(context.Background(), "paused", time.Now())
	if err == nil {
		t.Fatal("expected the disabled workflow's trigger to be rejected")
	}
	var rejected *TriggerRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected *TriggerRejectedError, got %T: %v", err, err)
	}
	if !strings.HasPrefix(rejected.Reason, ReasonDisabled) {
		t.Errorf("reason must carry the stable prefix %q: %q", ReasonDisabled, rejected.Reason)
	}
	if !strings.Contains(rejected.Reason, `spec.enabled=false`) {
		t.Errorf("reason must carry the named disabled diagnostic: %q", rejected.Reason)
	}
	if rejected.Transient() {
		t.Error("a disabled workflow is permanent until config changes, never transient")
	}
	if disabledStarter.count() != 0 {
		t.Errorf("the disabled workflow must not start, got %d run(s)", disabledStarter.count())
	}

	if _, err := sched.Trigger(context.Background(), "healthy", time.Now()); err != nil {
		t.Fatalf("the healthy workflow must keep serving, got %v", err)
	}
	waitForCount(t, func() int { return healthyStarter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawSkip bool
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && strings.HasPrefix(ev.Reason, ReasonDisabled) {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Errorf("expected a tick.skipped event with the disabled reason: %+v", events)
	}
}

func TestDisabledReasonSkippedByTick(t *testing.T) {
	counter := &countingBacklogCounter{}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:       "paused",
		Gaggle:         "example",
		Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:        &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}},
		BacklogCounter: counter,
		PollProvider:   apiv1.ProviderGitHub,
		DisabledReason: `gaggle "example" is disabled (spec.enabled=false)`,
	}})
	sched.Tick(context.Background(), time.Now())
	if counter.calls != 0 {
		t.Errorf("a disabled workflow must not spend provider polls, got %d", counter.calls)
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	skips := 0
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped {
			skips++
		}
	}
	if skips != 0 {
		t.Errorf("Tick must skip a disabled entry silently, got %d tick.skipped", skips)
	}
}
