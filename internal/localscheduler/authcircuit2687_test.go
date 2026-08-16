package localscheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

func TestAuthFailureCircuitStopsBacklogPollingUntilReload(t *testing.T) {
	authErr := errors.New("GET /commits/abc/check-runs failed: status 403: Resource not accessible by personal access token")
	failing := &fakeBacklogCounter{err: authErr}
	entry := WorkflowEntry{
		Workflow:              "implementation",
		Gaggle:                "goobers-site",
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		BacklogCounter:        failing,
		ScheduleDemandCounter: failing,
		Starter:               &fakeStarter{},
	}
	sched, dir := newTestScheduler(t, []WorkflowEntry{entry})
	now := time.Now()

	sched.Tick(context.Background(), now.Add(2*time.Hour))
	sched.Tick(context.Background(), now.Add(3*time.Hour))
	if got := failing.polls(); got != 1 {
		t.Fatalf("polls after permanent auth failure = %d, want 1", got)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var authEvents int
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == providers.ErrorCodeAuthFailed {
			authEvents++
		}
	}
	if authEvents != 1 {
		t.Fatalf("github_auth_failed events = %d, want 1: %+v", authEvents, events)
	}

	repaired := &fakeBacklogCounter{}
	entry.BacklogCounter = repaired
	entry.Schedules = nil
	entry.ScheduleDemandCounter = nil
	if err := sched.Reload([]WorkflowEntry{entry}, nil, now, "old", "new"); err != nil {
		t.Fatal(err)
	}
	sched.Tick(context.Background(), now.Add(4*time.Hour))
	if got := repaired.polls(); got != 1 {
		t.Fatalf("polls after credential configuration reload = %d, want 1", got)
	}
}

func TestAuthFailureCircuitStopsRunRedispatch(t *testing.T) {
	for _, failureCode := range []string{
		providers.ErrorCodeAuthFailed,
		telemetry.ErrCodeCredentialUnavailable,
	} {
		t.Run(failureCode, func(t *testing.T) {
			starter := &fakeStarter{result: StartResult{
				Phase:          journal.PhaseFailed,
				FailureStage:   "query-backlog",
				FailureCode:    failureCode,
				FailureMessage: "permission denied",
			}}
			sched, _ := newTestScheduler(t, []WorkflowEntry{{
				Workflow:  "implementation",
				Gaggle:    "goobers-site",
				Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
				Starter:   starter,
			}})

			if _, err := sched.Trigger(context.Background(), "implementation", time.Now()); err != nil {
				t.Fatal(err)
			}
			identity := WorkflowIdentity{Gaggle: "goobers-site", Workflow: "implementation"}
			waitForCount(t, func() int {
				if sched.authCircuitOpen(identity) {
					return 1
				}
				return 0
			}, 1)

			_, err := sched.Trigger(context.Background(), "implementation", time.Now())
			var rejected *TriggerRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("second trigger error = %v, want TriggerRejectedError", err)
			}
			if !strings.HasPrefix(rejected.Reason, ReasonProviderAuth) {
				t.Fatalf("second trigger reason = %q, want %q prefix", rejected.Reason, ReasonProviderAuth)
			}
			if got := starter.count(); got != 1 {
				t.Fatalf("run starts after permanent auth failure = %d, want 1", got)
			}
		})
	}
}
