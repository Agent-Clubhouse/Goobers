package readservice

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TestRunOutcomeReflectsTerminalGateDecision proves the #851 business-
// decision axis: a completed run's Outcome names the last gate evaluated
// before completion and its verdict/target, distinct from Phase (the
// execution axis #849 fixed).
func TestRunOutcomeReflectsTerminalGateDecision(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-outcome-gate", machine.Def.Name, "goobers",
		time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	appendEvent := func(event journal.Event) {
		t.Helper()
		clock.advance(time.Second)
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendEvent(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)})
	appendEvent(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: "local-ci"})
	var gateSeq uint64
	events, err := service.RunEvents(context.Background(), "run-outcome-gate")
	if err == nil {
		for _, event := range events.Events {
			if event.Type == journal.EventGateEvaluated {
				gateSeq = event.Seq
			}
		}
	}
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)

	detail, err := service.GetRun(context.Background(), "run-outcome-gate")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Outcome == nil ||
		detail.Outcome.Gate != "review" ||
		detail.Outcome.Verdict != "pass" ||
		detail.Outcome.Target != "local-ci" {
		t.Fatalf("outcome = %+v", detail.Outcome)
	}
	if gateSeq != 0 && detail.Outcome.CausalEventSeq != gateSeq {
		t.Fatalf("causal event = %d, want gate seq %d", detail.Outcome.CausalEventSeq, gateSeq)
	}
}

// TestRunOutcomeNilForNonCompletedRun proves Outcome is only meaningful for
// Phase == completed — a running or escalated run's Outcome is nil, never a
// zero-value struct that could be mistaken for "no gate decided this."
func TestRunOutcomeNilForNonCompletedRun(t *testing.T) {
	service, layout, machine := fixtureService(t)

	running, clock := createFixtureRun(
		t, layout, machine, "run-outcome-running", machine.Def.Name, "goobers",
		time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock.advance(time.Second)
	if err := running.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	detail, err := service.GetRun(context.Background(), "run-outcome-running")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Outcome != nil {
		t.Fatalf("running run outcome = %+v, want nil", detail.Outcome)
	}

	escalated, clock2 := createFixtureRun(
		t, layout, machine, "run-outcome-escalated", machine.Def.Name, "goobers",
		time.Date(2026, 7, 25, 14, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock2.advance(time.Second)
	if err := escalated.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	clock2.advance(time.Second)
	if err := escalated.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultFailure), Error: &journal.ErrorDetail{Code: "boom"},
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, escalated, clock2, journal.PhaseEscalated)
	detail, err = service.GetRun(context.Background(), "run-outcome-escalated")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Outcome != nil {
		t.Fatalf("escalated run outcome = %+v, want nil", detail.Outcome)
	}
}

// TestRunOutcomeEmptyForGatelessCompletion proves a completed run whose path
// evaluated no gate at all still gets a non-nil, all-empty RunOutcome — the
// axis is meaningful (Phase is completed), it just has no gate to report,
// which nil could not distinguish from "not computed."
func TestRunOutcomeEmptyForGatelessCompletion(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-outcome-gateless", machine.Def.Name, "goobers",
		time.Date(2026, 7, 25, 15, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)

	detail, err := service.GetRun(context.Background(), "run-outcome-gateless")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Outcome == nil {
		t.Fatal("outcome = nil, want a non-nil all-empty RunOutcome")
	}
	if detail.Outcome.Gate != "" || detail.Outcome.Verdict != "" || detail.Outcome.Target != "" {
		t.Fatalf("gateless outcome = %+v, want all empty", detail.Outcome)
	}
}

// TestRunOutcomeUsesCurrentLifecycleSegmentAfterResume proves Outcome
// attributes to the post-resume gate decision, not a stale pre-resume one —
// the same currentLifecycleRecords segmentation escalationCause already
// applies for the escalated case.
func TestRunOutcomeUsesCurrentLifecycleSegmentAfterResume(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-outcome-resumed", machine.Def.Name, "goobers",
		time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	appendEvent := func(event journal.Event) {
		t.Helper()
		clock.advance(time.Second)
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendEvent(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultFailure)})
	appendEvent(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "escalate"})
	appendEvent(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)})
	appendEvent(journal.Event{
		Type: journal.EventRunResumed, Status: string(journal.PhaseEscalated), Target: "implement",
		Actor: "operator@example.test", WorkflowVersion: machine.Def.Version, WorkflowDigest: machine.Digest(),
	})
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendEvent(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)})
	appendEvent(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: "local-ci"})
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)

	detail, err := service.GetRun(context.Background(), "run-outcome-resumed")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Outcome == nil || detail.Outcome.Verdict != "pass" || detail.Outcome.Target != "local-ci" {
		t.Fatalf("outcome after resume = %+v, want the post-resume pass/local-ci decision", detail.Outcome)
	}
}

func TestRunOutcomeUsesGateOverrideDecision(t *testing.T) {
	tests := []struct {
		name            string
		overrideVerdict string
		overrideTarget  string
		continueRun     bool
		wantVerdict     string
		wantTarget      string
		wantEvent       journal.EventType
	}{
		{
			name:            "terminal pass",
			overrideVerdict: "pass",
			overrideTarget:  "@complete",
			wantVerdict:     "pass",
			wantTarget:      "@complete",
			wantEvent:       journal.EventGateOverridden,
		},
		{
			name:            "nonterminal branch",
			overrideVerdict: "needs-changes",
			overrideTarget:  "implement",
			continueRun:     true,
			wantVerdict:     "pass",
			wantTarget:      "@complete",
			wantEvent:       journal.EventGateEvaluated,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, layout, machine := fixtureService(t)
			runID := "run-outcome-override-" + test.overrideVerdict
			run, clock := createFixtureRun(
				t, layout, machine, runID, machine.Def.Name, "goobers",
				time.Date(2026, 7, 25, 17, 0, 0, 0, time.UTC),
				journal.Trigger{Kind: journal.TriggerManual}, false,
			)
			appendEvent := func(event journal.Event) {
				t.Helper()
				clock.advance(time.Second)
				if err := run.Append(event); err != nil {
					t.Fatal(err)
				}
			}
			appendEvent(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "@escalate"})
			appendEvent(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)})
			appendEvent(journal.Event{
				Type: journal.EventGateOverridden, Gate: "review",
				Verdict: test.overrideVerdict, Target: test.overrideTarget,
				Actor: "operator@example.test", Rationale: "Manually reviewed the gate.",
				Status:          string(journal.PhaseEscalated),
				WorkflowVersion: machine.Def.Version, WorkflowDigest: machine.Digest(),
			})
			if test.continueRun {
				appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
				appendEvent(journal.Event{
					Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
					Status: string(apiv1.ResultSuccess),
				})
				appendEvent(journal.Event{
					Type: journal.EventGateEvaluated, Gate: "review",
					Verdict: "pass", Target: "@complete",
				})
			}
			finishFixtureRun(t, run, clock, journal.PhaseCompleted)

			detail, err := service.GetRun(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Outcome == nil ||
				detail.Outcome.Gate != "review" ||
				detail.Outcome.Verdict != test.wantVerdict ||
				detail.Outcome.Target != test.wantTarget {
				t.Fatalf("outcome = %+v, want review/%s/%s", detail.Outcome, test.wantVerdict, test.wantTarget)
			}
			events, err := service.RunEvents(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			var wantSeq uint64
			for _, event := range events.Events {
				if event.Type == test.wantEvent && event.Verdict == test.wantVerdict {
					wantSeq = event.Seq
				}
			}
			if wantSeq == 0 || detail.Outcome.CausalEventSeq != wantSeq {
				t.Fatalf("causal event = %d, want %s seq %d", detail.Outcome.CausalEventSeq, test.wantEvent, wantSeq)
			}
		})
	}
}
