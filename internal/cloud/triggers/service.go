// Package triggers is the cloud PostgreSQL trigger plane. It implements the
// API's TriggerService and TriggerStatusService over the shared acceptance
// store, on the API pool as goobers_api with no controller credentials.
//
// Acceptance is the whole API-side job: an ordinary trigger is admitted
// against the applied configuration generation, pinned to that generation's
// digests, and committed to control.acceptance. Draining accepted rows through
// the start fence, uncertain-start reconciliation and retries belong to the
// single-active controller (internal/cloud/control, startfence), so nothing
// here schedules or dispatches.
package triggers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrPinUnavailable means workflow digests cannot be pinned, so no start can be
// admitted. Acceptance fails closed (the caller retries with the same key).
var ErrPinUnavailable = errors.New("cloud triggers: workflow digest pinning is not available")

// Store is the acceptance custody the plane needs from the API pool.
type Store interface {
	Accept(ctx context.Context, key, actor string, payload []byte, now time.Time) (triggerqueue.Record, bool, error)
	Get(ctx context.Context, id, actor string) (triggerqueue.Record, error)
}

// Admission is the applied configuration's engine-only guard and generation.
type Admission interface {
	// Generation returns the applied generation digest, or "" before the first apply.
	Generation() string
	// Admit refuses a workflow the applied generation did not assess eligible.
	Admit(digest, gaggle, workflow string) error
}

// Pin returns the immutable digests a start is admitted against.
type Pin func(ctx context.Context, generation, gaggle, workflow string) (workflowDigest, gooberDigest string, err error)

// Service accepts triggers and reports acceptance status.
type Service struct {
	Store     Store
	Admission func() Admission
	Pin       Pin
	Now       func() time.Time
}

var (
	_ httpapi.TriggerService       = (*Service)(nil)
	_ httpapi.TriggerStatusService = (*Service)(nil)
)

func invalid(message string) error {
	return httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, message, nil)
}

func unavailable(cause error) error {
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, "trigger_acceptance_unavailable", "trigger could not be acknowledged; retry with the same key", cause)
}

// Trigger durably accepts one trigger request. A repeated request key returns
// the original acceptance; reusing a key for a different request is a conflict.
func (s *Service) Trigger(ctx context.Context, request httpapi.TriggerRequest) (httpapi.TriggerResponse, error) {
	request.RequestID = strings.TrimSpace(request.RequestID)
	switch {
	case strings.TrimSpace(request.Workflow) == "" || strings.TrimSpace(request.Gaggle) == "":
		return httpapi.TriggerResponse{}, invalid("trigger must name its workflow and gaggle")
	case request.RequestID == "":
		return httpapi.TriggerResponse{}, invalid("trigger requires a request key")
	case request.Force && (request.SourceRun != "" || request.PodScoped):
		return httpapi.TriggerResponse{}, invalid("force is invalid for priority and pod-scoped triggers")
	case request.PodScoped && request.PodRunID == "":
		return httpapi.TriggerResponse{}, invalid("a pod-scoped trigger must name its run")
	}
	var admission Admission
	if s.Admission != nil {
		admission = s.Admission()
	}
	if s.Store == nil || admission == nil || s.Pin == nil {
		return httpapi.TriggerResponse{}, unavailable(ErrPinUnavailable)
	}
	generation := admission.Generation()
	if generation == "" {
		return httpapi.TriggerResponse{}, unavailable(errors.New("no configuration generation is applied"))
	}
	if err := admission.Admit(generation, request.Gaggle, request.Workflow); err != nil {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(http.StatusUnprocessableEntity, "trigger_workflow_ineligible", err.Error(), nil)
	}
	workflowDigest, gooberDigest, err := s.Pin(ctx, generation, request.Gaggle, request.Workflow)
	if err != nil {
		return httpapi.TriggerResponse{}, unavailable(err)
	}
	now := s.now()
	raw, err := (startintent.Envelope{
		Kind: startintent.Kind,
		Request: startintent.Request{
			Workflow: request.Workflow, Gaggle: request.Gaggle, SourceRun: request.SourceRun, Force: request.Force,
			PodScoped: request.PodScoped, PodRunID: request.PodRunID,
		},
		Target: startintent.Target{
			Gaggle: request.Gaggle, Workflow: request.Workflow, ConfigGeneration: generation,
			WorkflowDigest: workflowDigest, GooberDigest: gooberDigest,
		},
		Deadline: now.Add(startintent.DefaultPendingAge).UTC(),
	}).Marshal()
	if err != nil {
		return httpapi.TriggerResponse{}, err
	}
	record, duplicate, err := s.Store.Accept(ctx, request.RequestID, request.Actor, raw, now)
	if errors.Is(err, triggerqueue.ErrConflict) {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(http.StatusConflict, "trigger_request_conflict", "request key belongs to another trigger", nil)
	}
	if err != nil {
		return httpapi.TriggerResponse{}, unavailable(err)
	}
	return httpapi.TriggerResponse{AcceptanceID: record.ID, State: string(record.State), RunID: record.RunID, Duplicate: duplicate}, nil
}

// TriggerStatus reports one actor-scoped acceptance receipt. A receipt that
// belongs to another actor, or to a different pod scope, reads as not found.
func (s *Service) TriggerStatus(ctx context.Context, request httpapi.TriggerStatusRequest) (httpapi.TriggerStatusResponse, error) {
	missing := httpapi.NewInterventionError(http.StatusNotFound, "trigger_not_found", "trigger acceptance not found", nil)
	if s.Store == nil || len(request.AcceptanceID) > 128 {
		return httpapi.TriggerStatusResponse{}, missing
	}
	record, err := s.Store.Get(ctx, request.AcceptanceID, request.Actor)
	if errors.Is(err, sql.ErrNoRows) {
		return httpapi.TriggerStatusResponse{}, missing
	}
	if err != nil {
		return httpapi.TriggerStatusResponse{}, err
	}
	envelope, err := startintent.Parse(record.Payload)
	if err != nil {
		return httpapi.TriggerStatusResponse{}, err
	}
	if envelope.Request.PodScoped != request.PodScoped || (request.PodScoped && envelope.Request.PodRunID != request.PodRunID) {
		return httpapi.TriggerStatusResponse{}, missing
	}
	return httpapi.TriggerStatusResponse{AcceptanceID: record.ID, State: string(record.State), RunID: record.RunID, Reason: record.Reason, AcceptedAt: record.AcceptedAt}, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
