package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/enginestartintent"
	"github.com/goobers/goobers/internal/eventexecution"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactivesession"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// durableTriggerService separates HTTP acceptance from scheduler availability.
// Only the daemon sweep calls Drain, after startup admission has opened.
type durableTriggerService struct {
	directEngine           *enginestartintent.Service
	directEngineCursor     string
	ordinary               *startintent.Service
	sessions               *interactivesession.Service
	sessionCursor          string
	childFamilies          *childFamilyLifecycle
	queue                  *triggerqueue.Store
	dispatch               *daemonTriggerService
	sweepMu                sync.Mutex
	reconcileCursor        string
	bootUncertain          map[string]bool
	auditLog               *journal.InstanceLog
	observe                func(context.Context, triggerqueue.Record) (bool, error)
	children               childExecutionLauncher
	observeChild           childStartObserver
	childCursor            string
	events                 *eventexecution.Service
	eventPublicationCursor string
	eventScopeCursor       string
	eventGroupCursor       string
}

// The wire request deliberately excludes authority fields. Persist them in a
// separate envelope so reopening the queue cannot turn a pod into an operator.
type acceptedTriggerPayload struct {
	Request   httpapi.TriggerRequest `json:"request"`
	PodScoped bool                   `json:"podScoped"`
	PodRunID  string                 `json:"podRunId"`
}

func newDaemonCoordinationServices(layout instance.Layout, dispatch *daemonTriggerService, runners *daemonRunnerRegistry, auditLog *journal.InstanceLog) (*durableTriggerService, *daemonStateService, *daemonCancelService, error) {
	if auditLog == nil {
		return nil, nil, nil, errors.New("trigger dispatch requires an instance audit journal")
	}
	state, err := newDaemonStateService(layout)
	if err != nil {
		return nil, nil, nil, err
	}
	triggers, err := newDurableTriggerService(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), dispatch)
	if err != nil {
		return nil, nil, nil, err
	}
	cancels, err := newPersistentDaemonCancelService(layout, runners, auditLog)
	if err != nil {
		_ = triggers.queue.Close()
		return nil, nil, nil, err
	}
	triggers.observe = acceptedTriggerObserver(layout)
	triggers.observeChild = acceptedChildObserver(layout)
	triggers.auditLog = auditLog
	triggers.childFamilies = &childFamilyLifecycle{layout: layout, queue: triggers.queue, runners: runners}
	cancels.fenceChildren = triggers.childFamilies.Fence
	return triggers, state, cancels, nil
}

func newDurableTriggerService(path string, dispatch *daemonTriggerService) (*durableTriggerService, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	queue, err := triggerqueue.Open(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uncertain, err := queue.UncertainIDs(ctx)
	if err != nil {
		_ = queue.Close()
		return nil, err
	}
	return &durableTriggerService{queue: queue, dispatch: dispatch, bootUncertain: uncertain}, nil
}

func (s *durableTriggerService) Trigger(ctx context.Context, request httpapi.TriggerRequest) (httpapi.TriggerResponse, error) {
	if err := s.dispatch.validateTriggerAuthority(request); err != nil {
		return httpapi.TriggerResponse{}, err
	}
	request.RequestID = strings.TrimSpace(request.RequestID)
	if strings.TrimSpace(request.Workflow) == "" || (request.SourceRun != "" && request.Gaggle == "") {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, "trigger must name its workflow and priority gaggle", nil)
	}
	if response, handled, err := s.acceptOrdinary(ctx, request); handled {
		return response, err
	}
	payload, err := json.Marshal(acceptedTriggerPayload{Request: request, PodScoped: request.PodScoped, PodRunID: request.PodRunID})
	if err != nil {
		return httpapi.TriggerResponse{}, err
	}
	record, duplicate, err := s.queue.Accept(ctx, request.RequestID, request.Actor, payload, s.dispatch.now())
	if errors.Is(err, triggerqueue.ErrConflict) {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(http.StatusConflict, "trigger_request_conflict", "request key belongs to another trigger", nil)
	}
	if err != nil {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(http.StatusServiceUnavailable, "trigger_acceptance_unavailable", "trigger could not be acknowledged; retry with the same key", err)
	}
	return httpapi.TriggerResponse{AcceptanceID: record.ID, State: string(record.State), RunID: record.RunID, Duplicate: duplicate}, nil
}

func (s *durableTriggerService) TriggerStatus(ctx context.Context, request httpapi.TriggerStatusRequest) (httpapi.TriggerStatusResponse, error) {
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
	podScoped, podRunID, err := acceptedTriggerScope(record.Payload)
	if err != nil {
		return httpapi.TriggerStatusResponse{}, err
	}
	if podScoped != request.PodScoped || (request.PodScoped && podRunID != request.PodRunID) {
		return httpapi.TriggerStatusResponse{}, missing
	}
	return httpapi.TriggerStatusResponse{AcceptanceID: record.ID, State: string(record.State), RunID: record.RunID, Reason: record.Reason, AcceptedAt: record.AcceptedAt}, nil
}

// Drain takes a bounded batch with durable per-record claims. In-flight records
// are never replayed here: crash reconciliation must prove whether a run exists
// before releasing their custody. The request's HTTP context is never used.
func (s *durableTriggerService) Drain(ctx context.Context) error {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	// Child custody shares the ordinary queue database. Retention must run
	// even while no scheduler is attached; otherwise inactive installations
	// retain terminal lineages and cancellation fences indefinitely.
	pruneCtx, cancelPrune := context.WithTimeout(ctx, 250*time.Millisecond)
	_, pruneErr := s.queue.PruneChildren(pruneCtx, s.dispatch.now(), 100)
	cancelPrune()
	eventPruneCtx, cancelEventPrune := context.WithTimeout(ctx, 50*time.Millisecond)
	_, eventPruneErr := s.queue.PruneEvents(eventPruneCtx, s.dispatch.now(), 100)
	cancelEventPrune()
	pruneErr = errors.Join(pruneErr, eventPruneErr, s.pruneWorkbenchCommands(ctx))
	if s.childFamilies != nil {
		pruneErr = errors.Join(pruneErr, s.childFamilies.Sweep(ctx))
	}
	pruneErr = errors.Join(pruneErr, s.sweepEvents(ctx), s.sweepSessions(ctx), s.sweepDirectEngine(ctx))
	if s.dispatch.triggerer() == nil && s.children == nil && s.events == nil && s.sessions == nil {
		return pruneErr
	}
	reconcileErr := errors.Join(pruneErr, s.reconcileObserved(ctx), s.reconcileChildren(ctx))
	records, err := s.queue.Pending(ctx, 100)
	if err != nil {
		return errors.Join(reconcileErr, err)
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		reconcileErr = errors.Join(reconcileErr, s.drainOne(ctx, record))
	}
	return reconcileErr
}

func (s *durableTriggerService) drainOne(ctx context.Context, record triggerqueue.Record) error {
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(record.Payload, &header); err != nil {
		return fmt.Errorf("decode accepted trigger %s: %w", record.ID, err)
	}
	// Generated children require their own pinned-definition launcher. Never
	// treat a generated workflow's display name as a catalog trigger, even if
	// an envelope also contains an ordinary request. Retain durable custody.
	if header.Kind == enginestartintent.Kind {
		return s.drainDirectEngine(ctx, record)
	}
	if header.Kind == startintent.Kind {
		return s.drainOrdinary(ctx, record)
	}
	if header.Kind == childworkflow.ChildStartKind {
		return s.drainChild(ctx, record)
	}
	if header.Kind == sessioning.StartKind {
		if s.sessions == nil {
			return nil
		}
		return s.sessions.Dispatch(ctx, s.dispatch.lifecycleContext(ctx), record)
	}
	if header.Kind == eventing.StartKind {
		if s.events == nil {
			return nil
		}
		return s.events.Dispatch(ctx, s.dispatch.lifecycleContext(ctx), record)
	}
	if header.Kind != "" {
		return fmt.Errorf("accepted trigger %s has an unsupported envelope kind", record.ID)
	}
	if s.dispatch.triggerer() == nil {
		return nil
	}
	var payload acceptedTriggerPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return fmt.Errorf("decode accepted trigger %s: %w", record.ID, err)
	}
	if err := s.auditDispatch(record, payload.Request); err != nil {
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
			if rejected.Transient() {
				return s.queue.Requeue(ctx, record.ID, reason)
			}
		}
		return s.queue.Finish(ctx, record.ID, triggerqueue.Rejected, "", reason, s.dispatch.now())
	}
	return s.queue.RecordDispatch(ctx, record.ID, response.RunID)
}

// A dispatch-attempt annotation may repeat after a crash between journal append
// and claiming the queue record. The acceptance ID correlates those attempts;
// the annotation never claims that execution has already started.
func (s *durableTriggerService) auditDispatch(record triggerqueue.Record, request httpapi.TriggerRequest) error {
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

func acceptedTriggerScope(raw []byte) (bool, string, error) {
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return false, "", err
	}
	if header.Kind == startintent.Kind {
		e, err := startintent.Parse(raw)
		return e.Request.PodScoped, e.Request.PodRunID, err
	}
	var payload acceptedTriggerPayload
	err := json.Unmarshal(raw, &payload)
	return payload.PodScoped, payload.PodRunID, err
}
