package intervention

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// TestEscalationDenyJournalsResolutionAndStaysTerminal covers the HITL
// plane's deny: the resolution event is journaled by the run's own journal
// writer, the run stays escalated, a replay under the same Idempotency-Key
// returns without a second event, and key reuse with a different payload is
// refused.
func TestEscalationDenyJournalsResolutionAndStaysTerminal(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-deny", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	adapter := NewEscalationResolver(service)
	ctx := context.Background()
	input := httpapi.EscalationResolutionRequest{
		RunID:          "run-deny",
		IdempotencyKey: "deny-1",
		Actor:          "operator",
		Resolution:     httpapi.EscalationResolutionDeny,
		Rationale:      "not shippable",
	}
	first, err := adapter.AcceptResolve(ctx, ctx, input)
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	if first.Phase != string(journal.PhaseEscalated) {
		t.Fatalf("deny result phase = %q, want the run to stay escalated", first.Phase)
	}
	second, err := adapter.AcceptResolve(ctx, ctx, input)
	if err != nil {
		t.Fatalf("replayed deny: %v", err)
	}
	if second != first {
		t.Fatalf("replay = %+v, first = %+v", second, first)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	resolutions := 0
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == escalationResolutionMarker {
			resolutions++
			if event.Runner["resolution"] != "deny" || event.Runner["actor"] != "operator" || event.Runner["rationale"] != "not shippable" {
				t.Fatalf("resolution event = %+v", event.Runner)
			}
		}
	}
	if resolutions != 1 {
		t.Fatalf("resolution events = %d, want exactly one", resolutions)
	}

	input.Rationale = "different rationale"
	_, err = adapter.AcceptResolve(ctx, ctx, input)
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) || interventionErr.Code != "idempotency_key_reused" {
		t.Fatalf("key reuse error = %#v, want idempotency_key_reused", err)
	}
}

// TestEscalationDenyConcurrentSameKeyAppendsOnce is the stale-snapshot
// regression: AcceptDenyEscalation's replay scan runs on a journal snapshot
// taken before the active-intervention slot is acquired, so two concurrent
// same-key denies could both pass the scan and both append
// escalation.resolution. The race is modeled deterministically: both denies
// resolve the run before either appends (the interleave the probe reproduced
// under scheduling pressure), then serialize through the slot — the second
// must detect the first's marker under the slot and replay it instead of
// appending a second event.
func TestEscalationDenyConcurrentSameKeyAppendsOnce(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-deny-race", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	input := httpapi.InterventionRequest{
		RunID:          "run-deny-race",
		IdempotencyKey: "deny-race-1",
		Actor:          "operator",
		Rationale:      "not shippable",
	}

	// Both deliveries snapshot the journal before either appends.
	staleA, err := service.resolve("run-deny-race")
	if err != nil {
		t.Fatal(err)
	}
	staleB, err := service.resolve("run-deny-race")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.denyEscalation(staleA, input)
	if err != nil {
		t.Fatalf("first deny: %v", err)
	}
	second, err := service.denyEscalation(staleB, input)
	if err != nil {
		t.Fatalf("racing deny with a stale snapshot: %v", err)
	}
	if second != first {
		t.Fatalf("racing deny = %+v, want the first result %+v replayed", second, first)
	}

	// A full-path replay still answers the recorded result.
	adapter := NewEscalationResolver(service)
	ctx := context.Background()
	replay, err := adapter.AcceptResolve(ctx, ctx, httpapi.EscalationResolutionRequest{
		RunID:          "run-deny-race",
		IdempotencyKey: "deny-race-1",
		Actor:          "operator",
		Resolution:     httpapi.EscalationResolutionDeny,
		Rationale:      "not shippable",
	})
	if err != nil || replay != first {
		t.Fatalf("replay = %+v, err = %v, want %+v", replay, err, first)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	resolutions := 0
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == escalationResolutionMarker {
			resolutions++
		}
	}
	if resolutions != 1 {
		t.Fatalf("resolution events = %d, want exactly one", resolutions)
	}
}

func TestEscalationAdapterValidatesResolutionInputs(t *testing.T) {
	adapter := NewEscalationResolver(nil)
	ctx := context.Background()
	tests := []struct {
		name     string
		input    httpapi.EscalationResolutionRequest
		wantCode string
	}{
		{
			name:     "approve without gate",
			input:    httpapi.EscalationResolutionRequest{RunID: "r", Resolution: httpapi.EscalationResolutionApprove},
			wantCode: "gate_required",
		},
		{
			name:     "redirect without decision",
			input:    httpapi.EscalationResolutionRequest{RunID: "r", Resolution: httpapi.EscalationResolutionRedirect, Gate: "review"},
			wantCode: "decision_required",
		},
		{
			name:     "unknown resolution",
			input:    httpapi.EscalationResolutionRequest{RunID: "r", Resolution: "park"},
			wantCode: "invalid_resolution",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := adapter.AcceptResolve(ctx, ctx, test.input)
			var interventionErr *httpapi.InterventionError
			if !errors.As(err, &interventionErr) || interventionErr.Code != test.wantCode {
				t.Fatalf("err = %#v, want code %s", err, test.wantCode)
			}
		})
	}
}
