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

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// durableTriggerService separates HTTP acceptance from scheduler availability.
// Only the daemon sweep calls Drain, after startup admission has opened.
type durableTriggerService struct {
	queue           *triggerqueue.Store
	dispatch        *daemonTriggerService
	sweepMu         sync.Mutex
	reconcileCursor string
	bootUncertain   map[string]bool
	observe         func(context.Context, triggerqueue.Record) (bool, error)
}

// The wire request deliberately excludes authority fields. Persist them in a
// separate envelope so reopening the queue cannot turn a pod into an operator.
type acceptedTriggerPayload struct {
	Request   httpapi.TriggerRequest `json:"request"`
	PodScoped bool                   `json:"podScoped"`
	PodRunID  string                 `json:"podRunId"`
}

func newDaemonCoordinationServices(layout instance.Layout, dispatch *daemonTriggerService) (*durableTriggerService, *daemonStateService, error) {
	state, err := newDaemonStateService(layout)
	if err != nil {
		return nil, nil, err
	}
	triggers, err := newDurableTriggerService(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), dispatch)
	if err != nil {
		return nil, nil, err
	}
	triggers.observe = acceptedTriggerObserver(layout)
	return triggers, state, nil
}

func newDurableTriggerService(path string, dispatch *daemonTriggerService) (*durableTriggerService, error) {
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
	var payload acceptedTriggerPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return httpapi.TriggerStatusResponse{}, err
	}
	if payload.PodScoped != request.PodScoped || (request.PodScoped && payload.PodRunID != request.PodRunID) {
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
	if s.dispatch.triggerer() == nil {
		return nil
	}
	reconcileErr := s.reconcileObserved(ctx)
	records, err := s.queue.Pending(ctx, 100)
	if err != nil {
		return errors.Join(reconcileErr, err)
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.drainOne(ctx, record); err != nil {
			return errors.Join(reconcileErr, err)
		}
	}
	return reconcileErr
}

func (s *durableTriggerService) drainOne(ctx context.Context, record triggerqueue.Record) error {
	var payload acceptedTriggerPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return fmt.Errorf("decode accepted trigger %s: %w", record.ID, err)
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
		return s.queue.Finish(ctx, record.ID, triggerqueue.Rejected, "", "scheduler refused the accepted trigger", s.dispatch.now())
	}
	return s.queue.RecordDispatch(ctx, record.ID, response.RunID)
}
