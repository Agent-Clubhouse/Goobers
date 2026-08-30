package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// writeplanes.go implements the daemon side of the write API's claims and
// trigger planes (distributed-state-and-coordination.md §7) plus the HITL
// resolution adapter. Nothing here is a second coordinator: the claims plane
// opens the same claims.json under the same cross-process flock the CLI
// claimants use (withClaimLock, providercmd.go), and the trigger plane calls
// the same Scheduler.Trigger* methods the pending-triggers sweep dispatches
// through (rundelegate.go) — the file seams remain for local/mode-1 callers.

// API claims-plane lock operation labels, alongside providercmd.go's.
const (
	claimLockOperationAPIAcquire = "api.claims.acquire"
	claimLockOperationAPIRenew   = "api.claims.renew"
	claimLockOperationAPIRelease = "api.claims.release"
	claimLockOperationAPISettle  = "api.claims.settle"
	claimLockOperationAPIList    = "api.claims.list"
)

// claimSettleOutcomes is the closed vocabulary for settle's outcome field.
var claimSettleOutcomes = map[string]bool{"completed": true, "abandoned": true}

// daemonClaimService is the claims plane over the daemon's claim ledger. The
// ledger file stays the store (DS3): every operation opens it fresh under the
// cross-process claims lock, exactly as the CLI provider-chain subcommands
// do, so API callers and subprocess callers share one atomicity domain.
type daemonClaimService struct {
	layout instance.Layout
	log    *journal.InstanceLog
}

func newDaemonClaimService(layout instance.Layout, log *journal.InstanceLog) *daemonClaimService {
	return &daemonClaimService{layout: layout, log: log}
}

func (s *daemonClaimService) lockPath() string {
	return filepath.Join(s.layout.SchedulerDir(), claimLockFileName)
}

func (s *daemonClaimService) ledgerPath() string {
	return filepath.Join(s.layout.SchedulerDir(), claimLedgerFileName)
}

func (s *daemonClaimService) leaseDuration(request httpapi.ClaimRequest) (time.Duration, error) {
	if request.LeaseSeconds < 0 {
		return 0, httpapi.NewInterventionError(http.StatusBadRequest, "invalid_lease", "leaseSeconds must not be negative", nil)
	}
	// The route already refuses over-cap leases; the service repeats the check
	// so no other assembly can mint an unbounded (or, past ~9.3e9 seconds, a
	// negative-overflow) lease — a 400, never a 500 write_failed.
	if request.LeaseSeconds > httpapi.MaxClaimLeaseSeconds {
		return 0, httpapi.NewInterventionError(http.StatusBadRequest, "invalid_lease",
			fmt.Sprintf("leaseSeconds must not exceed %d", httpapi.MaxClaimLeaseSeconds), nil)
	}
	if request.LeaseSeconds == 0 {
		return DefaultClaimLease, nil
	}
	return time.Duration(request.LeaseSeconds) * time.Second, nil
}

func claimKey(request httpapi.ClaimRequest) localscheduler.ClaimKey {
	return localscheduler.ClaimKey{
		Gaggle:     request.Gaggle,
		Provider:   request.Provider,
		ExternalID: request.ItemID,
	}
}

// withLedger runs fn against a freshly-opened ledger under the cross-process
// claims lock — the same fresh-open-under-flock discipline every subprocess
// claimant uses, which is what makes the API and the file-seam callers one
// atomicity domain rather than two.
func (s *daemonClaimService) withLedger(operation string, request httpapi.ClaimRequest, fn func(*localscheduler.ClaimLedger) error) error {
	return withClaimLockForRun(s.lockPath(), operation, request.Gaggle, request.RunID, func() error {
		opts := []localscheduler.LedgerOption{}
		if s.log != nil {
			opts = append(opts, localscheduler.WithInstanceLog(s.log))
		}
		ledger, err := localscheduler.OpenClaimLedger(s.ledgerPath(), opts...)
		if err != nil {
			return err
		}
		return fn(ledger)
	})
}

// Acquire claims the item for the requesting run. Refusal (a live lease held
// by another run) is not an error: it answers Ok=false with the holder, and
// journals claim.refused so both outcomes of a two-claimant race are
// observable (§13 item 2). An idempotent re-claim by the same run renews.
func (s *daemonClaimService) Acquire(_ context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	lease, err := s.leaseDuration(request)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	var response httpapi.ClaimResponse
	err = s.withLedger(claimLockOperationAPIAcquire, request, func(ledger *localscheduler.ClaimLedger) error {
		ok, holder, err := ledger.ClaimScoped(claimKey(request), request.RunID, request.Workflow, lease)
		if err != nil {
			return err
		}
		if !ok {
			response = httpapi.ClaimResponse{Ok: false, Holder: holder}
			s.journalRefusal(request, holder)
			return nil
		}
		expires := time.Now().Add(lease)
		response = httpapi.ClaimResponse{Ok: true, ExpiresAt: &expires}
		return nil
	})
	return response, err
}

// Renew extends the requesting run's own lease. Ok=false reports a claim
// that is no longer the run's to renew (released, reaped, or reassigned) —
// stale work for the caller to stop, not an error (RenewEntry's contract).
func (s *daemonClaimService) Renew(_ context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	lease, err := s.leaseDuration(request)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	var response httpapi.ClaimResponse
	err = s.withLedger(claimLockOperationAPIRenew, request, func(ledger *localscheduler.ClaimLedger) error {
		ok, err := ledger.RenewEntry(localscheduler.ClaimEntry{
			Gaggle:     request.Gaggle,
			Provider:   request.Provider,
			ExternalID: request.ItemID,
			ItemID:     request.ItemID,
			RunID:      request.RunID,
			Workflow:   request.Workflow,
		}, lease)
		if err != nil {
			return err
		}
		if !ok {
			holder := ""
			if entry, held := ledger.LookupScoped(claimKey(request)); held {
				holder = entry.RunID
			}
			response = httpapi.ClaimResponse{Ok: false, Holder: holder}
			return nil
		}
		expires := time.Now().Add(lease)
		response = httpapi.ClaimResponse{Ok: true, ExpiresAt: &expires}
		return nil
	})
	return response, err
}

// Release gives the item back mid-run. Releasing a claim not held is a
// no-op, not an error — the ledger's own idempotency contract. With ItemID
// empty it releases every claim the run holds (narrowed to the namespace
// when one is given) — the plane's form of releaseClaimsForRun, contained
// to the caller's own run exactly like the single-item shape.
func (s *daemonClaimService) Release(_ context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	if request.ItemID == "" {
		return s.releaseAllForRun(request)
	}
	return s.release(claimLockOperationAPIRelease, request, nil)
}

func (s *daemonClaimService) releaseAllForRun(request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	if (request.Gaggle == "") != (request.Provider == "") {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest,
			"gaggle and provider must be given together for a release of every claim the run holds", nil)
	}
	var released []httpapi.ClaimEntry
	err := s.withLedger(claimLockOperationAPIRelease, request, func(ledger *localscheduler.ClaimLedger) error {
		for _, entry := range ledger.ForRunAll(request.RunID) {
			if request.Gaggle != "" && (entry.Gaggle != request.Gaggle || entry.Provider != request.Provider) {
				continue
			}
			if err := ledger.ReleaseEntry(entry, request.RunID); err != nil {
				return err
			}
			released = append(released, claimEntryWire(entry))
		}
		return nil
	})
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	return httpapi.ClaimResponse{Ok: true, Released: released}, nil
}

// List reads the ledger for the caller. A namespace listing includes the
// ledger's legacy unscoped entries beside the namespace's own, because an
// unresolved item-only claim is exclusive against every scoped claimant
// (ClaimLedger.claim) — a lister that could not see it would select an item
// acquire is going to refuse. History is the retained released set for the
// same namespace, newest first, so the failure-streak deprioritization an
// off-daemon backlog-query runs keeps its input.
func (s *daemonClaimService) List(_ context.Context, request httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
	if request.RunID == "" {
		return httpapi.ClaimListResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, "runId is required", nil)
	}
	switch request.Scope {
	case httpapi.ClaimListScopeRun:
	case httpapi.ClaimListScopeNamespace:
		if request.Gaggle == "" || request.Provider == "" {
			return httpapi.ClaimListResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest,
				"gaggle and provider are required for a namespace listing", nil)
		}
		if request.PodScoped && !s.runBelongsToGaggle(request.Gaggle, request.RunID) {
			// Containment beyond the run id: a pod may read the namespace its
			// run was admitted into and no other. The run's journal on this
			// instance is the authority on which gaggle that is (the same
			// lookup the credential plane's locateRun makes).
			return httpapi.ClaimListResponse{}, httpapi.NewInterventionError(http.StatusForbidden, "gaggle_mismatch",
				"pod principal may only list the gaggle namespace its own run belongs to", nil)
		}
	default:
		return httpapi.ClaimListResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, "scope must be run or namespace", nil)
	}
	inNamespace := func(entry localscheduler.ClaimEntry) bool {
		if entry.Gaggle == "" && entry.Provider == "" {
			return true // legacy unscoped claims are exclusive against every namespace
		}
		return entry.Gaggle == request.Gaggle && entry.Provider == request.Provider
	}
	var response httpapi.ClaimListResponse
	err := s.withLedger(claimLockOperationAPIList, httpapi.ClaimRequest{Gaggle: request.Gaggle, RunID: request.RunID}, func(ledger *localscheduler.ClaimLedger) error {
		var entries, history []localscheduler.ClaimEntry
		switch request.Scope {
		case httpapi.ClaimListScopeRun:
			entries = ledger.ForRunAll(request.RunID)
			if request.IncludeHistory {
				history = ledger.HistoryForRun(request.RunID)
			}
		case httpapi.ClaimListScopeNamespace:
			for _, entry := range ledger.Snapshot() {
				if inNamespace(entry) {
					entries = append(entries, entry)
				}
			}
			if request.IncludeHistory {
				for _, entry := range ledger.HistorySnapshot() {
					if inNamespace(entry) {
						history = append(history, entry)
					}
				}
			}
		}
		response.Entries = claimEntriesWire(entries)
		response.History = claimEntriesWire(history)
		return nil
	})
	return response, err
}

// runBelongsToGaggle reports whether runID's journal lives under gaggle's
// runs directory on this instance. Delegates to the shared containment check
// (schedulerstate.go), which the scheduler-state plane applies to the same
// question for the same reason.
func (s *daemonClaimService) runBelongsToGaggle(gaggle, runID string) bool {
	return runBelongsToGaggle(s.layout, gaggle, runID)
}

// plainPathElement reports whether value is safe to join as exactly one path
// segment: non-empty, no separator of either platform's flavour, no volume
// name, and not a relative-path element.
func plainPathElement(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	if strings.ContainsAny(value, `/\`) || filepath.VolumeName(value) != "" {
		return false
	}
	return true
}

func claimEntryWire(entry localscheduler.ClaimEntry) httpapi.ClaimEntry {
	return httpapi.ClaimEntry{
		ItemID:     entry.ItemID,
		Gaggle:     entry.Gaggle,
		Provider:   entry.Provider,
		ExternalID: entry.ExternalID,
		RunID:      entry.RunID,
		Workflow:   entry.Workflow,
		ClaimedAt:  entry.ClaimedAt,
		ExpiresAt:  entry.ExpiresAt,
		ReleasedAt: entry.ReleasedAt,
	}
}

func claimEntriesWire(entries []localscheduler.ClaimEntry) []httpapi.ClaimEntry {
	if len(entries) == 0 {
		return nil
	}
	wire := make([]httpapi.ClaimEntry, 0, len(entries))
	for _, entry := range entries {
		wire = append(wire, claimEntryWire(entry))
	}
	return wire
}

// Settle is the exactly-once terminal release: the run concluded its work on
// the item (outcome completed/abandoned) and surrenders the lease. The lease
// plus the ledger's release idempotency are what make it exactly-once — a
// retried settle after the release is a no-op, and a settle by a run that
// lost the lease cannot release the new holder's claim. The provider-visible
// marker stays owned by the provider-chain stages (it mirrors the ledger and
// is never the source of truth).
func (s *daemonClaimService) Settle(_ context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	outcome := strings.TrimSpace(request.Outcome)
	if outcome == "" {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, "outcome_required", "settle requires an outcome", nil)
	}
	if !claimSettleOutcomes[outcome] {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, "invalid_outcome", "settle outcome must be completed or abandoned", nil)
	}
	return s.release(claimLockOperationAPISettle, request, map[string]any{
		"settled":       true,
		"settleOutcome": outcome,
	})
}

func (s *daemonClaimService) release(operation string, request httpapi.ClaimRequest, settleRunner map[string]any) (httpapi.ClaimResponse, error) {
	var response httpapi.ClaimResponse
	err := s.withLedger(operation, request, func(ledger *localscheduler.ClaimLedger) error {
		entry, held := ledger.LookupScoped(claimKey(request))
		releases := held && entry.RunID == request.RunID
		if err := ledger.ReleaseScoped(claimKey(request), request.RunID); err != nil {
			return err
		}
		response = httpapi.ClaimResponse{Ok: true}
		if settleRunner != nil && releases && s.log != nil {
			// The ledger journals claim.released; the settle disposition is
			// the plane's own annotation on the same instance journal, and
			// only the settle that actually surrendered the lease records it
			// — a retried settle is a silent no-op, which is the exactly-once
			// contract.
			_ = s.log.Append(journal.Event{
				Type:     journal.EventClaimReleased,
				Name:     request.ItemID,
				Gaggle:   request.Gaggle,
				RunID:    request.RunID,
				Workflow: request.Workflow,
				Runner:   settleRunner,
			})
		}
		return nil
	})
	return response, err
}

// journalRefusal records the losing side of a claim race. Best-effort like
// the ledger's own claim journaling: the refusal answer is authoritative
// either way.
func (s *daemonClaimService) journalRefusal(request httpapi.ClaimRequest, holder string) {
	if s.log == nil {
		return
	}
	_ = s.log.Append(journal.Event{
		Type:     journal.EventClaimRefused,
		Name:     request.ItemID,
		Gaggle:   request.Gaggle,
		RunID:    request.RunID,
		Workflow: request.Workflow,
		Runner: map[string]any{
			"claimProvider":   request.Provider,
			"claimExternalId": request.ItemID,
			"holder":          holder,
		},
	})
}

// workflowTriggerer is the slice of *localscheduler.Scheduler the trigger
// plane dispatches through — the same methods the pending-triggers sweep
// calls, seam-shaped for tests.
type workflowTriggerer interface {
	// Every method here is a *WithDispatchContext form, which splits the caller's context in two:
	// admission is validated against the CALLER's context (so a hung or
	// abandoned caller cannot hold an admission decision open), while the
	// dispatched run's own lifecycle hangs off the DAEMON's context.
	//
	// #3876 (decision 005 D1). Before the split, every trigger-plane dispatch
	// ran under the HTTP *request* context. An engine dispatch is
	// asynchronous — it starts a Temporal workflow, then awaits its result —
	// so `curl` hanging up, or Go's http.Server cancelling the request
	// context the instant the handler returns 200, cancelled the await. The
	// workflow kept running on the far side while this daemon concluded the
	// run had failed: a divergence between the durable truth and the
	// journal, produced by nothing more than a client disconnect. The runner
	// path had the same latent bug, hidden only because its dispatch was
	// synchronous enough to finish first.
	//
	// TriggerPriority* is the output-driven re-tick the sweep dispatches for
	// a priority request file (rundelegate.go) — the plane's path for a stage
	// pod, which has no scheduler directory to drop that file into.
	TriggerWithDispatchContext(ctx, dispatchCtx context.Context, workflow string, now time.Time) (string, error)
	TriggerExactWithDispatchContext(ctx, dispatchCtx context.Context, identity localscheduler.WorkflowIdentity, now time.Time) (string, error)
	TriggerPriorityWithDispatchContext(ctx, dispatchCtx context.Context, identity localscheduler.WorkflowIdentity, sourceRun string, now time.Time) (string, error)
}

// maxTriggerDedupeRecords bounds the trigger plane's delivery-dedupe memory,
// mirroring the webhook handler's bounded in-memory set (daemon-local is
// sound under DS1).
const maxTriggerDedupeRecords = 10000

// daemonTriggerService ingests external triggers: validate, dedupe, and mint
// through the exact scheduler path the poll-loop/sweep uses. The scheduler
// is attached after construction (the HTTP handler is built before the
// scheduler exists at daemon startup), mirroring
// runInterventionService.AttachScheduler.
type daemonTriggerService struct {
	sched atomic.Pointer[localscheduler.Scheduler]
	now   func() time.Time
	// dispatchCtx is the daemon's lifecycle context, attached alongside the
	// scheduler. Runs this plane mints live and die with the daemon, not with
	// the HTTP request that asked for them. nil (never attached) degrades to
	// the request context, which is the pre-#3876 behaviour.
	dispatchCtx atomic.Pointer[dispatchContextHolder]
	// dispatch overrides the scheduler dispatch seam in tests; nil dispatches
	// through the attached scheduler.
	dispatch workflowTriggerer
	// contains verifies that a pod caller's run belongs to the gaggle it
	// named — the trigger plane's half of decision 005 R3's "a pod principal
	// may POST /triggers for its OWN gaggle". nil is fail-closed: a pod
	// request is refused rather than admitted unverified.
	contains func(gaggle, runID string) bool

	mu    sync.Mutex
	seen  map[string]string // requestId -> minted run id
	order []string
}

func newDaemonTriggerService() *daemonTriggerService {
	return &daemonTriggerService{now: time.Now, seen: make(map[string]string)}
}

// withGaggleContainment attaches the pod-principal containment check. The
// daemon wires the instance's own run.yaml lookup here, the same authority the
// claims and scheduler-state planes use.
func (s *daemonTriggerService) withGaggleContainment(contains func(gaggle, runID string) bool) *daemonTriggerService {
	s.contains = contains
	return s
}

func (s *daemonTriggerService) AttachScheduler(sched *localscheduler.Scheduler) {
	if s != nil {
		s.sched.Store(sched)
	}
}

// dispatchContextHolder boxes a context for atomic.Pointer, which cannot hold
// an interface value.
type dispatchContextHolder struct{ ctx context.Context }

// AttachDispatchContext gives the plane the daemon's lifecycle context. Call
// it with the same context the scheduler itself runs under.
func (s *daemonTriggerService) AttachDispatchContext(ctx context.Context) {
	if s != nil && ctx != nil {
		s.dispatchCtx.Store(&dispatchContextHolder{ctx: ctx})
	}
}

// lifecycleContext returns the attached daemon context, falling back to the
// caller's when the plane was never attached.
func (s *daemonTriggerService) lifecycleContext(requestCtx context.Context) context.Context {
	if holder := s.dispatchCtx.Load(); holder != nil && holder.ctx != nil {
		return holder.ctx
	}
	return requestCtx
}

func (s *daemonTriggerService) triggerer() workflowTriggerer {
	if s.dispatch != nil {
		return s.dispatch
	}
	if sched := s.sched.Load(); sched != nil {
		return sched
	}
	return nil
}

// Trigger validates, dedupes, and mints. A redelivered RequestID whose
// original delivery minted a run answers that run without minting a second
// one; a delivery that failed does not poison its RequestID, so the caller
// may retry it. The dedupe is an atomic test-and-set BEFORE dispatch (the
// webhook handler's seen() discipline): the first delivery of a RequestID
// reserves it under the lock, so a concurrent duplicate gets the Duplicate
// response instead of racing the check-then-record window into a second mint.
func (s *daemonTriggerService) Trigger(ctx context.Context, request httpapi.TriggerRequest) (httpapi.TriggerResponse, error) {
	dispatch := s.triggerer()
	if dispatch == nil {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(
			http.StatusServiceUnavailable, "scheduler_unavailable", "run admission is not available", nil,
		)
	}
	// Pod containment (decision 005 R3). The route has already established
	// that the caller named a gaggle and, for a priority re-tick, its own run;
	// this is the authority check the route cannot make — does that run
	// actually live in that gaggle on this instance? A missing verifier is a
	// refusal, not a pass: the daemon must never admit a pod trigger it could
	// not contain.
	if request.PodScoped {
		if s.contains == nil {
			return httpapi.TriggerResponse{}, httpapi.NewInterventionError(
				http.StatusForbidden, "gaggle_mismatch",
				"pod-principal triggers are not available from this server", nil,
			)
		}
		if !s.contains(request.Gaggle, request.PodRunID) {
			return httpapi.TriggerResponse{}, httpapi.NewInterventionError(
				http.StatusForbidden, "gaggle_mismatch",
				"pod principal may only trigger a workflow in the gaggle its own run belongs to", nil,
			)
		}
	}
	requestID := strings.TrimSpace(request.RequestID)
	if requestID != "" {
		if runID, duplicate := s.reserve(requestID); duplicate {
			return httpapi.TriggerResponse{RunID: runID, Duplicate: true}, nil
		}
	}

	// The request context governs admission; the daemon's lifecycle context
	// governs the run the admission mints. See workflowTriggerer.
	dispatchCtx := s.lifecycleContext(ctx)

	var runID string
	var err error
	switch {
	case strings.TrimSpace(request.SourceRun) != "":
		// A priority re-tick names an exact workflow by construction: the
		// source run publishes durable state that changes ONE workflow's
		// selection order, and TriggerPriority takes that identity. An
		// unscoped priority request has no such identity, so it is refused
		// rather than resolved against whatever gaggle happens to match.
		if request.Gaggle == "" {
			err = httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest,
				"a priority trigger requires the workflow's gaggle", nil)
			break
		}
		runID, err = dispatch.TriggerPriorityWithDispatchContext(ctx, dispatchCtx, localscheduler.WorkflowIdentity{
			Gaggle: request.Gaggle, Workflow: request.Workflow,
		}, strings.TrimSpace(request.SourceRun), s.now())
	case request.Gaggle != "":
		runID, err = dispatch.TriggerExactWithDispatchContext(ctx, dispatchCtx, localscheduler.WorkflowIdentity{
			Gaggle: request.Gaggle, Workflow: request.Workflow,
		}, s.now())
	default:
		runID, err = dispatch.TriggerWithDispatchContext(ctx, dispatchCtx, request.Workflow, s.now())
	}
	if err != nil {
		if requestID != "" {
			s.releaseReservation(requestID)
		}
		return httpapi.TriggerResponse{}, triggerPlaneError(err)
	}
	if requestID != "" {
		s.completeReservation(requestID, runID)
	}
	return httpapi.TriggerResponse{RunID: runID}, nil
}

// reserve atomically claims requestID for the calling delivery. The winner
// (duplicate=false) must completeReservation with the minted run, or
// releaseReservation on mint failure so the delivery stays retryable. A
// duplicate reports the recorded run — empty while the winning delivery is
// still minting, which is still authoritatively "this delivery was already
// accepted".
func (s *daemonTriggerService) reserve(requestID string) (runID string, duplicate bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID, exists := s.seen[requestID]; exists {
		return runID, true
	}
	s.seen[requestID] = ""
	s.order = append(s.order, requestID)
	if len(s.order) > maxTriggerDedupeRecords {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.seen, oldest)
	}
	return "", false
}

// completeReservation fixes the minted run onto the reservation (unless the
// bounded record already evicted it).
func (s *daemonTriggerService) completeReservation(requestID, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.seen[requestID]; exists {
		s.seen[requestID] = runID
	}
}

// releaseReservation drops a reservation whose mint failed, keeping the
// RequestID retryable. Only an unfulfilled reservation is dropped: a recorded
// mint (or an entry the bounded record replaced) is left alone.
func (s *daemonTriggerService) releaseReservation(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID, exists := s.seen[requestID]; !exists || runID != "" {
		return
	}
	delete(s.seen, requestID)
	for i, id := range s.order {
		if id == requestID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// triggerPlaneError maps scheduler trigger failures onto typed API refusals:
// a transient capacity refusal is retryable (429), any other conditions
// refusal is a conflict (409), an unknown/ambiguous workflow is the caller's
// error, and everything else surfaces as a server fault.
func triggerPlaneError(err error) error {
	var rejected *localscheduler.TriggerRejectedError
	if errors.As(err, &rejected) {
		if rejected.Transient() {
			return httpapi.NewInterventionError(http.StatusTooManyRequests, "trigger_capacity", err.Error(), err)
		}
		return httpapi.NewInterventionError(http.StatusConflict, "trigger_rejected", err.Error(), err)
	}
	switch {
	case strings.Contains(err.Error(), "unknown workflow"):
		return httpapi.NewInterventionError(http.StatusNotFound, "workflow_not_found", err.Error(), err)
	case strings.Contains(err.Error(), "is ambiguous"):
		return httpapi.NewInterventionError(http.StatusBadRequest, "workflow_ambiguous", err.Error(), err)
	default:
		return err
	}
}

// escalationResolutionAdapter maps the HITL plane's resolution vocabulary
// (approve/deny/redirect) onto the intervention service's existing escalated-
// run operations, so the plane reuses the resume/override machinery — and its
// journaling — rather than forking it.
type escalationResolutionAdapter struct {
	interventions *runInterventionService
}

func newEscalationResolutionAdapter(interventions *runInterventionService) *escalationResolutionAdapter {
	return &escalationResolutionAdapter{interventions: interventions}
}

func (a *escalationResolutionAdapter) AcceptResolve(admission, execution context.Context, input httpapi.EscalationResolutionRequest) (httpapi.InterventionResult, error) {
	request := httpapi.InterventionRequest{
		RunID:          input.RunID,
		Stage:          input.Gate,
		IdempotencyKey: input.IdempotencyKey,
		Actor:          input.Actor,
		Decision:       input.Decision,
		Rationale:      input.Rationale,
	}
	switch input.Resolution {
	case httpapi.EscalationResolutionApprove:
		if strings.TrimSpace(input.Gate) == "" {
			return httpapi.InterventionResult{}, interventionBadRequest("gate_required", "approve requires the escalated gate")
		}
		return a.interventions.AcceptApprove(admission, execution, request)
	case httpapi.EscalationResolutionRedirect:
		if strings.TrimSpace(input.Gate) == "" {
			return httpapi.InterventionResult{}, interventionBadRequest("gate_required", "redirect requires the escalated gate")
		}
		if strings.TrimSpace(input.Decision) == "" {
			return httpapi.InterventionResult{}, interventionBadRequest("decision_required", "redirect requires a branch decision")
		}
		return a.interventions.AcceptOverride(admission, execution, request)
	case httpapi.EscalationResolutionDeny:
		return a.interventions.AcceptDenyEscalation(admission, execution, request)
	default:
		return httpapi.InterventionResult{}, interventionBadRequest(
			"invalid_resolution",
			fmt.Sprintf("resolution %q must be approve, deny, or redirect", input.Resolution),
		)
	}
}
