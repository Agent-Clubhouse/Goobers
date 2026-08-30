package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
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

// TestClaimsPlaneLeaseBounds pins the LeaseSeconds ceiling: at-cap leases are
// honored, over-cap and duration-overflow values are a named 400 (never a 500
// write_failed), and the zero-value default path is unchanged.
func TestClaimsPlaneLeaseBounds(t *testing.T) {
	service, _ := newClaimServiceFixture(t)
	ctx := context.Background()

	// The API cap tracks the effective default lease: 4× DefaultClaimLease.
	if want := int(4 * DefaultClaimLease / time.Second); httpapi.MaxClaimLeaseSeconds != want {
		t.Fatalf("MaxClaimLeaseSeconds = %d, want %d (4× DefaultClaimLease)", httpapi.MaxClaimLeaseSeconds, want)
	}

	// At the cap: accepted, and the expiry honors the requested lease.
	atCap := claimPlaneRequest("run-cap")
	atCap.LeaseSeconds = httpapi.MaxClaimLeaseSeconds
	before := time.Now()
	response, err := service.Acquire(ctx, atCap)
	if err != nil || !response.Ok || response.ExpiresAt == nil {
		t.Fatalf("at-cap acquire = %+v, err = %v", response, err)
	}
	wantLease := time.Duration(httpapi.MaxClaimLeaseSeconds) * time.Second
	if d := response.ExpiresAt.Sub(before); d < wantLease-time.Minute || d > wantLease+time.Minute {
		t.Fatalf("at-cap lease = %v, want ~%v", d, wantLease)
	}

	// Over-cap — and, on 64-bit ints, a value past the time.Duration overflow
	// range — is refused as a 400 with a named validation error.
	overflows := []int{httpapi.MaxClaimLeaseSeconds + 1}
	if strconv.IntSize == 64 {
		tenBillion := int64(10_000_000_000)
		overflows = append(overflows, int(tenBillion))
	}
	for _, lease := range overflows {
		bad := claimPlaneRequest("run-over")
		bad.LeaseSeconds = lease
		_, err := service.Acquire(ctx, bad)
		var planeErr *httpapi.InterventionError
		if !errors.As(err, &planeErr) || planeErr.Status != http.StatusBadRequest || planeErr.Code != "invalid_lease" {
			t.Fatalf("leaseSeconds %d: err = %#v, want 400 invalid_lease", lease, err)
		}
		if _, err := service.Renew(ctx, bad); !errors.As(err, &planeErr) || planeErr.Status != http.StatusBadRequest {
			t.Fatalf("renew with leaseSeconds %d: err = %#v, want 400 invalid_lease", lease, err)
		}
	}

	// The default path is unchanged: zero takes DefaultClaimLease.
	def := claimPlaneRequest("run-default")
	def.ItemID = "43"
	before = time.Now()
	response, err = service.Acquire(ctx, def)
	if err != nil || !response.Ok || response.ExpiresAt == nil {
		t.Fatalf("default acquire = %+v, err = %v", response, err)
	}
	if d := response.ExpiresAt.Sub(before); d < DefaultClaimLease-time.Minute || d > DefaultClaimLease+time.Minute {
		t.Fatalf("default lease = %v, want ~%v", d, DefaultClaimLease)
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

func (s *stubTriggerer) TriggerWithDispatchContext(_, _ context.Context, _ string, _ time.Time) (string, error) {
	return s.mint()
}

func (s *stubTriggerer) TriggerExactWithDispatchContext(_, _ context.Context, _ localscheduler.WorkflowIdentity, _ time.Time) (string, error) {
	return s.mint()
}

func (s *stubTriggerer) TriggerPriorityWithDispatchContext(_, _ context.Context, _ localscheduler.WorkflowIdentity, _ string, _ time.Time) (string, error) {
	return s.mint()
}

// barrierTriggerer blocks the FIRST mint inside the dispatch seam until
// released, so a test can deliver a duplicate while the winning delivery's
// mint is deterministically still in flight. Later mints pass straight
// through: a broken dedupe lets the duplicate reach the seam, which must
// surface as a second mint rather than a deadlock.
type barrierTriggerer struct {
	entered chan struct{}
	release chan struct{}
	mints   atomic.Int32
}

func (b *barrierTriggerer) mint() (string, error) {
	n := b.mints.Add(1)
	if n == 1 {
		close(b.entered)
		<-b.release
	}
	return fmt.Sprintf("run-%d", n), nil
}

func (b *barrierTriggerer) TriggerWithDispatchContext(_, _ context.Context, _ string, _ time.Time) (string, error) {
	return b.mint()
}

func (b *barrierTriggerer) TriggerExactWithDispatchContext(_, _ context.Context, _ localscheduler.WorkflowIdentity, _ time.Time) (string, error) {
	return b.mint()
}

func (b *barrierTriggerer) TriggerPriorityWithDispatchContext(_, _ context.Context, _ localscheduler.WorkflowIdentity, _ string, _ time.Time) (string, error) {
	return b.mint()
}

// TestTriggerPlaneConcurrentDuplicateDeliveriesMintOnce is the check-then-
// record regression: the dedupe must reserve the RequestID atomically BEFORE
// dispatch (the webhook handler's seen() discipline). Two concurrent
// deliveries of one RequestID — the winner held inside the dispatch seam by a
// barrier — must produce exactly one mint; the concurrent duplicate gets the
// Duplicate response instead of racing the recorded()/record() window into a
// second run.
func TestTriggerPlaneConcurrentDuplicateDeliveriesMintOnce(t *testing.T) {
	barrier := &barrierTriggerer{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseBarrier := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	defer releaseBarrier()

	service := newDaemonTriggerService()
	service.dispatch = barrier
	ctx := context.Background()
	request := httpapi.TriggerRequest{Gaggle: "example", Workflow: "implementation", RequestID: "delivery-race"}

	var first httpapi.TriggerResponse
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		first, firstErr = service.Trigger(ctx, request)
	}()
	<-barrier.entered

	// The winning delivery is now inside the dispatch seam, mid-mint. The
	// concurrent duplicate must be answered from the reservation, not minted.
	second, err := service.Trigger(ctx, request)
	if err != nil {
		t.Fatalf("concurrent duplicate: %v", err)
	}
	if !second.Duplicate {
		t.Fatalf("concurrent duplicate = %+v, want Duplicate=true", second)
	}
	if second.RunID != "" && second.RunID != "run-1" {
		t.Fatalf("concurrent duplicate run = %q", second.RunID)
	}

	releaseBarrier()
	<-done
	if firstErr != nil || first.RunID != "run-1" || first.Duplicate {
		t.Fatalf("winning delivery = %+v, err = %v", first, firstErr)
	}
	if mints := barrier.mints.Load(); mints != 1 {
		t.Fatalf("mints = %d, want exactly one", mints)
	}

	// A later redelivery answers the recorded run.
	replay, err := service.Trigger(ctx, request)
	if err != nil || !replay.Duplicate || replay.RunID != "run-1" {
		t.Fatalf("redelivery = %+v, err = %v", replay, err)
	}
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
	adapter := newEscalationResolutionAdapter(service)
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

// TestClaimsPlaneListScopesAndContainment pins the daemon's list read
// (finding 002 C1 / the critic's claims/list-history row): a run listing is
// ForRunAll; a namespace listing carries the namespace's current holders,
// the legacy unscoped entries the ledger holds exclusive against it, and —
// with history — the released entries (ReleasedAt, RunID) the
// failure-streak deprioritization reads; a pod-scoped namespace listing is
// confined to the gaggle the pod's run belongs to.
func TestClaimsPlaneListScopesAndContainment(t *testing.T) {
	service, layout := newClaimServiceFixture(t)
	ctx := context.Background()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	claim := func(gaggle, item, runID string) {
		t.Helper()
		if ok, _, err := ledger.ClaimScoped(localscheduler.ClaimKey{Gaggle: gaggle, Provider: "github", ExternalID: item}, runID, "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("seed claim %s/%s: ok=%v err=%v", gaggle, item, ok, err)
		}
	}
	claim("g", "1", "run-1")
	claim("g", "2", "run-1")
	claim("g", "3", "run-3")
	claim("other", "1", "run-9")
	if ok, _, err := ledger.Claim("legacy", "run-legacy", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed legacy claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.ReleaseScoped(localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "2"}, "run-1"); err != nil {
		t.Fatal(err)
	}
	for _, gaggle := range []string{"g", "other"} {
		run, err := journal.Create(layout.ForGaggle(gaggle).RunsDir(), journal.RunIdentity{RunID: "run-" + gaggle, Workflow: "w", Gaggle: gaggle}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}

	byRun, err := service.List(ctx, httpapi.ClaimListRequest{RunID: "run-1", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(byRun.Entries) != 1 || byRun.Entries[0].ItemID != "1" || len(byRun.History) != 2 {
		t.Fatalf("run listing = %+v, want item 1 held and two history entries", byRun)
	}

	namespace, err := service.List(ctx, httpapi.ClaimListRequest{Gaggle: "g", Provider: "github", RunID: "run-1", Scope: httpapi.ClaimListScopeNamespace, IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	items := map[string]string{}
	for _, entry := range namespace.Entries {
		items[entry.ItemID] = entry.RunID
	}
	if len(items) != 3 || items["1"] != "run-1" || items["3"] != "run-3" || items["legacy"] != "run-legacy" {
		t.Fatalf("namespace holders = %v, want g's 1 and 3 plus the legacy entry, never other's", items)
	}
	var releasedTwo bool
	for _, entry := range namespace.History {
		if entry.Gaggle == "other" {
			t.Fatalf("namespace history leaked another gaggle's entry: %+v", entry)
		}
		if entry.ItemID == "2" && entry.ReleasedAt != nil && entry.RunID == "run-1" {
			releasedTwo = true
		}
	}
	if !releasedTwo {
		t.Fatalf("namespace history = %+v, want item 2's release by run-1 with ReleasedAt", namespace.History)
	}
	bare, err := service.List(ctx, httpapi.ClaimListRequest{Gaggle: "g", Provider: "github", RunID: "run-1", Scope: httpapi.ClaimListScopeNamespace})
	if err != nil || len(bare.History) != 0 {
		t.Fatalf("listing without history = %+v, %v", bare, err)
	}

	// Pod containment: run-g lives in gaggle g, so it may list g and not other.
	if _, err := service.List(ctx, httpapi.ClaimListRequest{Gaggle: "g", Provider: "github", RunID: "run-g", Scope: httpapi.ClaimListScopeNamespace, PodScoped: true}); err != nil {
		t.Fatalf("pod listing its own gaggle: %v", err)
	}
	_, err = service.List(ctx, httpapi.ClaimListRequest{Gaggle: "other", Provider: "github", RunID: "run-g", Scope: httpapi.ClaimListScopeNamespace, PodScoped: true})
	var planeErr *httpapi.InterventionError
	if !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden || planeErr.Code != "gaggle_mismatch" {
		t.Fatalf("pod listing another gaggle: err = %v, want 403 gaggle_mismatch", err)
	}
	_, err = service.List(ctx, httpapi.ClaimListRequest{Gaggle: "g", Provider: "github", RunID: "never-admitted", Scope: httpapi.ClaimListScopeNamespace, PodScoped: true})
	if !errors.As(err, &planeErr) || planeErr.Code != "gaggle_mismatch" {
		t.Fatalf("pod listing for a run with no journal: err = %v, want gaggle_mismatch", err)
	}
	// The gaggle is a pod-supplied path segment on the containment check's
	// only filesystem probe: anything that is not one plain element is
	// refused before it is joined, never resolved.
	for _, gaggle := range []string{"../g", "g/../g", "./g", ".", "..", "sub/g", `sub\g`} {
		_, err = service.List(ctx, httpapi.ClaimListRequest{
			Gaggle: gaggle, Provider: "github", RunID: "run-g",
			Scope: httpapi.ClaimListScopeNamespace, PodScoped: true,
		})
		if !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden || planeErr.Code != "gaggle_mismatch" {
			t.Fatalf("pod listing gaggle %q: err = %v, want 403 gaggle_mismatch", gaggle, err)
		}
	}
	if _, err := service.List(ctx, httpapi.ClaimListRequest{Gaggle: "other", Provider: "github", RunID: "run-g", Scope: httpapi.ClaimListScopeNamespace}); err != nil {
		t.Fatalf("a human (not PodScoped) listing of another gaggle: %v", err)
	}
	for name, request := range map[string]httpapi.ClaimListRequest{
		"no run":                     {Scope: httpapi.ClaimListScopeRun},
		"bad scope":                  {RunID: "run-1", Scope: "all"},
		"namespace without provider": {RunID: "run-1", Gaggle: "g", Scope: httpapi.ClaimListScopeNamespace},
	} {
		if _, err := service.List(ctx, request); !errors.As(err, &planeErr) || planeErr.Status != http.StatusBadRequest {
			t.Errorf("%s: err = %v, want 400", name, err)
		}
	}
}

// TestClaimsPlaneReleaseAllForRun pins release with itemId omitted: every
// claim the run holds goes back (narrowed to a namespace when given), the
// surrendered entries are reported, other runs' claims are untouched, and
// the release is journaled per entry like the ledger's own.
func TestClaimsPlaneReleaseAllForRun(t *testing.T) {
	service, layout := newClaimServiceFixture(t)
	ctx := context.Background()
	acquire := func(gaggle, item, runID string) {
		t.Helper()
		response, err := service.Acquire(ctx, httpapi.ClaimRequest{Gaggle: gaggle, Provider: "github", ItemID: item, RunID: runID, Workflow: "implementation"})
		if err != nil || !response.Ok {
			t.Fatalf("acquire %s/%s for %s: %+v, %v", gaggle, item, runID, response, err)
		}
	}
	acquire("g", "1", "run-1")
	acquire("g", "2", "run-1")
	acquire("h", "1", "run-1")
	acquire("g", "3", "run-2")

	narrowed, err := service.Release(ctx, httpapi.ClaimRequest{Gaggle: "g", Provider: "github", RunID: "run-1"})
	if err != nil || !narrowed.Ok || len(narrowed.Released) != 2 {
		t.Fatalf("namespace-narrowed release-all = %+v, %v; want g's two claims", narrowed, err)
	}
	everything, err := service.Release(ctx, httpapi.ClaimRequest{RunID: "run-1"})
	if err != nil || !everything.Ok || len(everything.Released) != 1 || everything.Released[0].Gaggle != "h" {
		t.Fatalf("release-all = %+v, %v; want h's remaining claim", everything, err)
	}
	again, err := service.Release(ctx, httpapi.ClaimRequest{RunID: "run-1"})
	if err != nil || !again.Ok || len(again.Released) != 0 {
		t.Fatalf("repeated release-all = %+v, %v; want an idempotent no-op", again, err)
	}
	if _, err := service.Release(ctx, httpapi.ClaimRequest{Gaggle: "g", RunID: "run-1"}); err == nil {
		t.Fatal("half a namespace was accepted")
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if held := ledger.Snapshot(); len(held) != 1 || held[0].RunID != "run-2" {
		t.Fatalf("ledger after release-all = %+v, want only run-2's claim", held)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	released := 0
	for _, event := range events {
		if event.Type == journal.EventClaimReleased && event.RunID == "run-1" {
			released++
		}
	}
	if released != 3 {
		t.Fatalf("claim.released events for run-1 = %d, want 3", released)
	}
}
