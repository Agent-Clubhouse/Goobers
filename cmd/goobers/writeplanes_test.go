package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

func newClaimServiceFixture(t *testing.T) (*daemonClaimService, instance.Layout) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	return newDaemonClaimService(layout, instanceLog), layout
}

func claimPlaneRequest(runID string) httpapi.ClaimRequest {
	return httpapi.ClaimRequest{
		Gaggle:   "example",
		Provider: "github",
		ItemID:   "42",
		RunID:    runID,
		Workflow: "implementation",
	}
}

// TestClaimsPlaneConcurrentClaimantsExactlyOneWins is §13 item 2 at the
// service seam: two concurrent claimants for one backlog item — exactly one
// wins, both outcomes are journaled (claim.acquired for the winner,
// claim.refused naming the holder for the loser), and the ledger holds one
// lease.
func TestClaimsPlaneConcurrentClaimantsExactlyOneWins(t *testing.T) {
	service, layout := newClaimServiceFixture(t)

	responses := make([]httpapi.ClaimResponse, 2)
	errs := make([]error, 2)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < 2; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			responses[i], errs[i] = service.Acquire(context.Background(), claimPlaneRequest(fmt.Sprintf("run-%d", i)))
		}(i)
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("claimant %d: %v", i, err)
		}
	}
	winners := 0
	winner, loser := -1, -1
	for i, response := range responses {
		if response.Ok {
			winners++
			winner = i
		} else {
			loser = i
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d (responses %+v), want exactly one", winners, responses)
	}
	if holder := responses[loser].Holder; holder != fmt.Sprintf("run-%d", winner) {
		t.Fatalf("loser was told holder %q, want run-%d", holder, winner)
	}
	if responses[winner].ExpiresAt == nil || !responses[winner].ExpiresAt.After(time.Now()) {
		t.Fatalf("winner lease expiry = %v", responses[winner].ExpiresAt)
	}

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	acquired, refused := 0, 0
	for _, event := range events {
		switch event.Type {
		case journal.EventClaimAcquired:
			acquired++
			if event.RunID != fmt.Sprintf("run-%d", winner) {
				t.Fatalf("claim.acquired for %q, want winner run-%d", event.RunID, winner)
			}
		case journal.EventClaimRefused:
			refused++
			if event.RunID != fmt.Sprintf("run-%d", loser) {
				t.Fatalf("claim.refused for %q, want loser run-%d", event.RunID, loser)
			}
			if holder, _ := event.Runner["holder"].(string); holder != fmt.Sprintf("run-%d", winner) {
				t.Fatalf("claim.refused holder = %q", holder)
			}
		}
	}
	if acquired != 1 || refused != 1 {
		t.Fatalf("journaled outcomes: acquired = %d, refused = %d; want one each", acquired, refused)
	}
}

func TestClaimsPlaneRenewAndReleaseFollowLedgerIdempotency(t *testing.T) {
	service, _ := newClaimServiceFixture(t)
	ctx := context.Background()

	if _, err := service.Acquire(ctx, claimPlaneRequest("run-a")); err != nil {
		t.Fatal(err)
	}
	renewed, err := service.Renew(ctx, claimPlaneRequest("run-a"))
	if err != nil || !renewed.Ok {
		t.Fatalf("own renew = %+v, err = %v", renewed, err)
	}
	// A different run cannot renew the same lease and learns the holder.
	stale, err := service.Renew(ctx, claimPlaneRequest("run-b"))
	if err != nil || stale.Ok || stale.Holder != "run-a" {
		t.Fatalf("stale renew = %+v, err = %v", stale, err)
	}
	// Release by a non-holder is a no-op; release by the holder frees the item.
	if response, err := service.Release(ctx, claimPlaneRequest("run-b")); err != nil || !response.Ok {
		t.Fatalf("non-holder release = %+v, err = %v", response, err)
	}
	if renewed, err := service.Renew(ctx, claimPlaneRequest("run-a")); err != nil || !renewed.Ok {
		t.Fatalf("lease must survive a non-holder release: %+v, err = %v", renewed, err)
	}
	if response, err := service.Release(ctx, claimPlaneRequest("run-a")); err != nil || !response.Ok {
		t.Fatalf("holder release = %+v, err = %v", response, err)
	}
	acquired, err := service.Acquire(ctx, claimPlaneRequest("run-b"))
	if err != nil || !acquired.Ok {
		t.Fatalf("released item must be claimable: %+v, err = %v", acquired, err)
	}
}

func TestClaimsPlaneSettleReleasesExactlyOnce(t *testing.T) {
	service, layout := newClaimServiceFixture(t)
	ctx := context.Background()

	if _, err := service.Acquire(ctx, claimPlaneRequest("run-a")); err != nil {
		t.Fatal(err)
	}
	request := claimPlaneRequest("run-a")
	request.Outcome = "completed"
	settled, err := service.Settle(ctx, request)
	if err != nil || !settled.Ok {
		t.Fatalf("settle = %+v, err = %v", settled, err)
	}
	// A retried settle is a no-op, not an error (exactly-once via lease +
	// release idempotency).
	if settled, err := service.Settle(ctx, request); err != nil || !settled.Ok {
		t.Fatalf("retried settle = %+v, err = %v", settled, err)
	}
	// A settle by a run that lost the lease cannot release the new holder.
	if acquired, err := service.Acquire(ctx, claimPlaneRequest("run-b")); err != nil || !acquired.Ok {
		t.Fatalf("post-settle acquire = %+v, err = %v", acquired, err)
	}
	staleSettle := claimPlaneRequest("run-a")
	staleSettle.Outcome = "abandoned"
	if settled, err := service.Settle(ctx, staleSettle); err != nil || !settled.Ok {
		t.Fatalf("stale settle = %+v, err = %v", settled, err)
	}
	ledger, err := localscheduler.OpenClaimLedger(service.ledgerPath())
	if err != nil {
		t.Fatal(err)
	}
	entry, held := ledger.LookupScoped(localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "42"})
	if !held || entry.RunID != "run-b" {
		t.Fatalf("ledger entry = %+v held=%v, want run-b's live lease", entry, held)
	}

	// Missing/unknown outcomes are refused before touching the ledger.
	for _, outcome := range []string{"", "sideways"} {
		bad := claimPlaneRequest("run-b")
		bad.Outcome = outcome
		if _, err := service.Settle(ctx, bad); err == nil {
			t.Fatalf("settle with outcome %q must fail", outcome)
		}
	}

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	settleAnnotations := 0
	for _, event := range events {
		if event.Type == journal.EventClaimReleased && event.Runner["settleOutcome"] == "completed" {
			settleAnnotations++
		}
	}
	if settleAnnotations != 1 {
		t.Fatalf("settle annotations = %d, want exactly one", settleAnnotations)
	}
}

type stubTriggerer struct {
	mu    sync.Mutex
	mints int
	err   error
}

func (s *stubTriggerer) mint() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	s.mints++
	return fmt.Sprintf("run-%d", s.mints), nil
}

func (s *stubTriggerer) Trigger(context.Context, string, time.Time) (string, error) {
	return s.mint()
}

func (s *stubTriggerer) TriggerExact(context.Context, localscheduler.WorkflowIdentity, time.Time) (string, error) {
	return s.mint()
}

// TestTriggerPlaneDedupesRedeliveredRequests is the trigger dedupe test: a
// redelivered RequestID answers the originally-minted run instead of minting
// a second one, and a failed delivery does not poison its RequestID.
func TestTriggerPlaneDedupesRedeliveredRequests(t *testing.T) {
	stub := &stubTriggerer{}
	service := newDaemonTriggerService()
	service.dispatch = stub
	ctx := context.Background()

	request := httpapi.TriggerRequest{Gaggle: "example", Workflow: "implementation", RequestID: "delivery-1"}
	first, err := service.Trigger(ctx, request)
	if err != nil || first.RunID != "run-1" || first.Duplicate {
		t.Fatalf("first delivery = %+v, err = %v", first, err)
	}
	second, err := service.Trigger(ctx, request)
	if err != nil || second.RunID != "run-1" || !second.Duplicate {
		t.Fatalf("redelivery = %+v, err = %v", second, err)
	}
	if stub.mints != 1 {
		t.Fatalf("mints = %d, want exactly one", stub.mints)
	}

	// A distinct RequestID mints normally.
	third, err := service.Trigger(ctx, httpapi.TriggerRequest{Workflow: "implementation", RequestID: "delivery-2"})
	if err != nil || third.RunID != "run-2" || third.Duplicate {
		t.Fatalf("second delivery = %+v, err = %v", third, err)
	}

	// A failed delivery is retryable under the same RequestID.
	stub.err = &localscheduler.TriggerRejectedError{Workflow: "implementation", Reason: localscheduler.ReasonMaxParallel}
	if _, err := service.Trigger(ctx, httpapi.TriggerRequest{Workflow: "implementation", RequestID: "delivery-3"}); err == nil {
		t.Fatal("refused trigger must surface an error")
	}
	stub.err = nil
	retried, err := service.Trigger(ctx, httpapi.TriggerRequest{Workflow: "implementation", RequestID: "delivery-3"})
	if err != nil || retried.Duplicate || retried.RunID != "run-3" {
		t.Fatalf("retry after refusal = %+v, err = %v", retried, err)
	}
}

func TestTriggerPlaneMapsSchedulerRefusals(t *testing.T) {
	stub := &stubTriggerer{}
	service := newDaemonTriggerService()
	service.dispatch = stub
	ctx := context.Background()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "transient capacity",
			err:        &localscheduler.TriggerRejectedError{Workflow: "w", Reason: localscheduler.ReasonMaxParallel},
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "trigger_capacity",
		},
		{
			name:       "non-transient refusal",
			err:        &localscheduler.TriggerRejectedError{Workflow: "w", Reason: "budget-exhausted"},
			wantStatus: http.StatusConflict,
			wantCode:   "trigger_rejected",
		},
		{
			name:       "unknown workflow",
			err:        errors.New(`localscheduler: unknown workflow "w"`),
			wantStatus: http.StatusNotFound,
			wantCode:   "workflow_not_found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub.err = test.err
			_, err := service.Trigger(ctx, httpapi.TriggerRequest{Workflow: "w"})
			var planeErr *httpapi.InterventionError
			if !errors.As(err, &planeErr) || planeErr.Status != test.wantStatus || planeErr.Code != test.wantCode {
				t.Fatalf("err = %#v, want status %d code %s", err, test.wantStatus, test.wantCode)
			}
		})
	}
}

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
	adapter := newEscalationResolutionAdapter(service)
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

func TestEscalationAdapterValidatesResolutionInputs(t *testing.T) {
	adapter := newEscalationResolutionAdapter(nil)
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
