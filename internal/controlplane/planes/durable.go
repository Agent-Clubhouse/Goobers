package planes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// defaultChildPruneBudget bounds one sweep's child-retention pass.
const defaultChildPruneBudget = 250 * time.Millisecond

// DurableTriggerService separates HTTP acceptance from scheduler availability.
// Only the daemon sweep calls Drain, after startup admission has opened.
type DurableTriggerService struct {
	queue           *triggerqueue.Store
	dispatch        *TriggerService
	backend         DurableTriggerBackend
	sweepGuard      drainGuard
	reconcileCursor string
	bootUncertain   map[string]bool
	auditLog        *journal.InstanceLog
	observe         func(context.Context, triggerqueue.Record) (bool, error)
	// pruneBudget bounds each sweep's child-retention pass so it cannot stall dispatch;
	// zero means defaultChildPruneBudget.
	pruneBudget time.Duration
}

// AcceptedTriggerPayload deliberately excludes authority fields. Persist them in a
// separate envelope so reopening the queue cannot turn a pod into an operator.
type AcceptedTriggerPayload struct {
	Request   httpapi.TriggerRequest `json:"request"`
	PodScoped bool                   `json:"podScoped"`
	PodRunID  string                 `json:"podRunId"`
}

// DurableTriggerBackend supplies host-owned ordinary and child seams.
type DurableTriggerBackend interface {
	AcceptOrdinary(context.Context, httpapi.TriggerRequest) (httpapi.TriggerResponse, bool, error)
	DrainOrdinary(context.Context, triggerqueue.Record) error
	DrainChild(context.Context, triggerqueue.Record) error
	HasChildren() bool
	SweepChildren(context.Context) error
	ReconcileChildren(context.Context) error
	CanReconcileAccepted() bool
	ReconcileAccepted(context.Context, triggerqueue.Record, string) (*bool, error)
}

// NewDurableTriggerService opens the accepted-trigger queue.
func NewDurableTriggerService(path string, dispatch *TriggerService, backend DurableTriggerBackend) (*DurableTriggerService, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	queue, err := triggerqueue.Open(path)
	if err != nil {
		return nil, err
	}
	service, err := NewDurableTriggerServiceWithQueue(queue, dispatch, backend)
	if err != nil {
		_ = queue.Close()
		return nil, err
	}
	return service, nil
}

// NewDurableTriggerServiceWithQueue attaches to an already-open trigger queue.
func NewDurableTriggerServiceWithQueue(queue *triggerqueue.Store, dispatch *TriggerService, backend DurableTriggerBackend) (*DurableTriggerService, error) {
	if queue == nil {
		return nil, errors.New("durable trigger queue is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uncertain, err := queue.UncertainIDs(ctx)
	if err != nil {
		return nil, err
	}
	return &DurableTriggerService{queue: queue, dispatch: dispatch, backend: backend, bootUncertain: uncertain}, nil
}

// Queue returns the accepted-trigger store.
func (s *DurableTriggerService) Queue() *triggerqueue.Store { return s.queue }

// Dispatch returns the trigger admission plane.
func (s *DurableTriggerService) Dispatch() *TriggerService { return s.dispatch }

// SetAuditLog attaches the instance audit journal.
func (s *DurableTriggerService) SetAuditLog(log *journal.InstanceLog) { s.auditLog = log }

// AuditLog returns the attached instance audit journal.
func (s *DurableTriggerService) AuditLog() *journal.InstanceLog { return s.auditLog }

// SetObserver attaches legacy-run publication observation.
func (s *DurableTriggerService) SetObserver(observe func(context.Context, triggerqueue.Record) (bool, error)) {
	s.observe = observe
}

// Close closes the accepted-trigger store.
func (s *DurableTriggerService) Close() error { return s.queue.Close() }

// Trigger durably accepts one trigger request.
func (s *DurableTriggerService) Trigger(ctx context.Context, request httpapi.TriggerRequest) (httpapi.TriggerResponse, error) {
	if err := s.dispatch.ValidateTriggerAuthority(request); err != nil {
		return httpapi.TriggerResponse{}, err
	}
	request.RequestID = strings.TrimSpace(request.RequestID)
	if strings.TrimSpace(request.Workflow) == "" || (request.SourceRun != "" && request.Gaggle == "") {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, "trigger must name its workflow and priority gaggle", nil)
	}
	if response, handled, err := s.backend.AcceptOrdinary(ctx, request); handled {
		return response, err
	}
	payload, err := json.Marshal(AcceptedTriggerPayload{Request: request, PodScoped: request.PodScoped, PodRunID: request.PodRunID})
	if err != nil {
		return httpapi.TriggerResponse{}, err
	}
	record, duplicate, err := s.queue.AcceptAdmitted(ctx, request.RequestID, request.Actor, payload, s.dispatch.Now(), func() error {
		return s.dispatch.ValidateTriggerTarget(request)
	})
	var refusal *httpapi.InterventionError
	if errors.As(err, &refusal) {
		return httpapi.TriggerResponse{}, err
	}
	if errors.Is(err, triggerqueue.ErrConflict) {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(http.StatusConflict, "trigger_request_conflict", "request key belongs to another trigger", nil)
	}
	if err != nil {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(http.StatusServiceUnavailable, "trigger_acceptance_unavailable", "trigger could not be acknowledged; retry with the same key", err)
	}
	return httpapi.TriggerResponse{AcceptanceID: record.ID, State: string(record.State), RunID: record.RunID, Duplicate: duplicate}, nil
}

// TriggerStatus reports one actor-scoped acceptance receipt.
func (s *DurableTriggerService) TriggerStatus(ctx context.Context, request httpapi.TriggerStatusRequest) (httpapi.TriggerStatusResponse, error) {
	missing := httpapi.NewInterventionError(http.StatusNotFound, "trigger_not_found", "trigger acceptance not found", nil)
	if len(request.AcceptanceID) > 128 {
		return httpapi.TriggerStatusResponse{}, missing
	}
	record, err := s.queue.Get(ctx, request.AcceptanceID, request.Actor)
	if errors.Is(err, sql.ErrNoRows) {
		return httpapi.TriggerStatusResponse{}, missing
	}
	if err != nil {
		return httpapi.TriggerStatusResponse{}, err
	}
	var payload AcceptedTriggerPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return httpapi.TriggerStatusResponse{}, err
	}
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(record.Payload, &header); err != nil {
		return httpapi.TriggerStatusResponse{}, err
	}
	if header.Kind == startintent.Kind {
		envelope, err := startintent.Parse(record.Payload)
		if err != nil {
			return httpapi.TriggerStatusResponse{}, err
		}
		payload.PodScoped, payload.PodRunID = envelope.Request.PodScoped, envelope.Request.PodRunID
	}
	if payload.PodScoped != request.PodScoped || (request.PodScoped && payload.PodRunID != request.PodRunID) {
		return httpapi.TriggerStatusResponse{}, missing
	}
	return httpapi.TriggerStatusResponse{AcceptanceID: record.ID, State: string(record.State), RunID: record.RunID, Reason: record.Reason, AcceptedAt: record.AcceptedAt}, nil
}

// Drain takes a bounded batch with durable per-record claims. In-flight records
// are never replayed here: crash reconciliation must prove whether a run exists
// before releasing their custody. The request's HTTP context is never used.
func (s *DurableTriggerService) Drain(ctx context.Context) error {
	release, err := s.sweepGuard.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	// Child custody shares the ordinary queue database. Retention must run
	// even while no scheduler is attached; otherwise inactive installations
	// retain terminal lineages and cancellation fences indefinitely.
	budget := s.pruneBudget
	if budget <= 0 {
		budget = defaultChildPruneBudget
	}
	pruneCtx, cancelPrune := context.WithTimeout(ctx, budget)
	_, pruneErr := s.queue.PruneChildren(pruneCtx, s.dispatch.Now(), 100)
	cancelPrune()
	// Budget exhaustion means "resume next sweep", not a failed sweep: the
	// queue database has one connection that child launches also hold, so the
	// prune routinely waits longer than its budget. Real prune errors and
	// cancellation of the parent context still propagate.
	if errors.Is(pruneErr, context.DeadlineExceeded) && ctx.Err() == nil {
		pruneErr = nil
	}
	pruneErr = errors.Join(pruneErr, s.backend.SweepChildren(ctx))
	if s.dispatch.Triggerer() == nil && !s.backend.HasChildren() {
		return pruneErr
	}
	reconcileErr := errors.Join(pruneErr, s.ReconcileObserved(ctx), s.backend.ReconcileChildren(ctx))
	return errors.Join(reconcileErr, s.drainPending(ctx))
}

// Reserve only the candidate this pass is about to inspect. Reserving the whole
// batch up front can repeatedly skip uninspected records when passes end early.
func (s *DurableTriggerService) drainPending(ctx context.Context) error {
	seen := make(map[string]struct{}, 100)
	var failures error
	for range 100 {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		records, err := s.queue.PendingSweep(ctx, 1)
		if err != nil || len(records) == 0 {
			return errors.Join(failures, err)
		}
		record := records[0]
		if _, visited := seen[record.ID]; visited {
			return failures
		}
		seen[record.ID] = struct{}{}
		// One malformed or temporarily unavailable owner must not pin the
		// remaining candidates. Its custody remains unchanged.
		failures = errors.Join(failures, s.drainOne(ctx, record))
	}
	return failures
}

func (s *DurableTriggerService) drainOne(ctx context.Context, record triggerqueue.Record) error {
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(record.Payload, &header); err != nil {
		return fmt.Errorf("decode accepted trigger %s: %w", record.ID, err)
	}
	// Generated children require their own pinned-definition launcher. Never
	// treat a generated workflow's display name as a catalog trigger, even if
	// an envelope also contains an ordinary request. Retain durable custody.
	if header.Kind == childworkflow.ChildStartKind {
		return s.backend.DrainChild(ctx, record)
	}
	if header.Kind == startintent.Kind {
		return s.backend.DrainOrdinary(ctx, record)
	}
	if header.Kind != "" {
		return fmt.Errorf("accepted trigger %s has an unsupported envelope kind", record.ID)
	}
	if s.dispatch.Triggerer() == nil {
		return nil
	}
	var payload AcceptedTriggerPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return fmt.Errorf("decode accepted trigger %s: %w", record.ID, err)
	}
	if err := s.AuditDispatch(record, payload.Request); err != nil {
		return err
	}
	if err := s.queue.BeginDispatch(ctx, record.ID); err != nil {
		if errors.Is(err, triggerqueue.ErrTransition) {
			return nil
		}
		return err
	}
	request := payload.Request
	request.Actor, request.PodScoped, request.PodRunID = record.Actor, payload.PodScoped, payload.PodRunID
	// Durable queue custody replaces the dispatcher's process-local dedupe.
	request.RequestID = ""
	request.DispatchRunID = strings.TrimPrefix(record.ID, "trigger-")
	response, err := s.dispatch.Trigger(ctx, request)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		reason := err.Error()
		var rejected *localscheduler.TriggerRejectedError
		if errors.As(err, &rejected) {
			reason = rejected.Reason
			if rejected.WaitableForQueue() {
				return s.queue.Requeue(ctx, record.ID, reason)
			}
		}
		return s.queue.Finish(ctx, record.ID, triggerqueue.Rejected, "", reason, s.dispatch.Now())
	}
	return s.queue.RecordDispatch(ctx, record.ID, response.RunID)
}

// AuditDispatch records one dispatch-attempt annotation. It may repeat after a crash between journal append
// and claiming the queue record. The acceptance ID correlates those attempts;
// the annotation never claims that execution has already started.
func (s *DurableTriggerService) AuditDispatch(record triggerqueue.Record, request httpapi.TriggerRequest) error {
	if s.auditLog == nil {
		return nil
	} // Low-level service test seam only.
	return s.auditLog.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, Actor: record.Actor,
		Workflow: request.Workflow, Gaggle: request.Gaggle,
		RunID:  strings.TrimPrefix(record.ID, "trigger-"),
		Reason: "accepted trigger dispatch requested",
		Runner: map[string]any{
			"note": "trigger.dispatch.requested", "acceptanceId": record.ID,
			"requestId": record.Key, "force": request.Force, "sourceRun": request.SourceRun,
		},
	})
}
