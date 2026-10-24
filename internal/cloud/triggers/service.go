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

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
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
	ByKey(ctx context.Context, key string) (triggerqueue.Record, error)
	Get(ctx context.Context, id, actor string) (triggerqueue.Record, error)
}

// Admission is the applied configuration's engine-only guard.
type Admission interface {
	// AdmitApplied reads ONE snapshot of the applied generation and returns its
	// digest. It wraps cloudconfig.ErrNoAppliedGeneration before the first apply and the engine-only
	// ineligibility error for a workflow that generation did not assess eligible.
	AdmitApplied(gaggle, workflow string) (generation string, err error)
}

// Pin returns the immutable digests a start is admitted against.
type Pin func(ctx context.Context, generation, gaggle, workflow string) (workflowDigest, gooberDigest string, err error)

// Contains reports whether runID is a run of gaggle on this instance. It is the
// pod-containment verifier: a stage pod may only trigger into the gaggle its own
// run belongs to.
type Contains func(ctx context.Context, gaggle, runID string) (bool, error)

// Service accepts triggers and reports acceptance status.
type Service struct {
	Store     Store
	Admission func() Admission
	Pin       Pin
	// Contains verifies pod principals. Nil refuses every pod-scoped trigger
	// (fail closed), like the local plane without a verifier.
	Contains Contains
	Now      func() time.Time
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

func gaggleMismatch(message string) error {
	return httpapi.NewInterventionError(http.StatusForbidden, "gaggle_mismatch", message, nil)
}

// authorize ports the local plane's ValidateTriggerAuthority: pod containment
// first, failing closed without a verifier.
func (s *Service) authorize(ctx context.Context, request httpapi.TriggerRequest) error {
	if !request.PodScoped {
		return nil
	}
	if s.Contains == nil {
		return gaggleMismatch("pod-principal triggers are not available from this server")
	}
	ok, err := s.Contains(ctx, request.Gaggle, request.PodRunID)
	if err != nil {
		return unavailable(err)
	}
	if !ok {
		return gaggleMismatch("pod principal may only trigger a workflow in the gaggle its own run belongs to")
	}
	return nil
}

func startRequest(request httpapi.TriggerRequest) startintent.Request {
	return startintent.Request{
		Workflow: request.Workflow, Gaggle: request.Gaggle, SourceRun: request.SourceRun, Force: request.Force,
		PodScoped: request.PodScoped, PodRunID: request.PodRunID,
	}
}

// replay answers an existing key from custody, exactly as the local plane does:
// admission, pinning and the current generation are never consulted, and the
// stored deadline and pins are kept. Only the actor and the parsed request are
// compared; the stored bytes carry a deadline and pins that legitimately differ
// between attempts. found is false when the key is unused.
func (s *Service) replay(ctx context.Context, request httpapi.TriggerRequest) (response httpapi.TriggerResponse, found bool, err error) {
	record, err := s.Store.ByKey(ctx, request.RequestID)
	if errors.Is(err, sql.ErrNoRows) {
		return httpapi.TriggerResponse{}, false, nil
	}
	if err != nil {
		return httpapi.TriggerResponse{}, true, unavailable(err)
	}
	envelope, err := startintent.Parse(record.Payload)
	if err != nil || record.Actor != request.Actor || envelope.Request != startRequest(request) {
		return httpapi.TriggerResponse{}, true, httpapi.NewInterventionError(http.StatusConflict, "trigger_request_conflict", "request key belongs to another trigger", nil)
	}
	return httpapi.TriggerResponse{AcceptanceID: record.ID, State: string(record.State), RunID: record.RunID, Duplicate: true}, true, nil
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
	if s.Store == nil {
		return httpapi.TriggerResponse{}, unavailable(errors.New("cloud triggers: no acceptance store"))
	}
	if err := s.authorize(ctx, request); err != nil {
		return httpapi.TriggerResponse{}, err
	}
	if response, found, err := s.replay(ctx, request); found {
		return response, err
	}
	var admission Admission
	if s.Admission != nil {
		admission = s.Admission()
	}
	if admission == nil {
		return httpapi.TriggerResponse{}, unavailable(errors.New("cloud triggers: no applied configuration is composed"))
	}
	if s.Pin == nil {
		return httpapi.TriggerResponse{}, unavailable(ErrPinUnavailable)
	}
	generation, err := admission.AdmitApplied(request.Gaggle, request.Workflow)
	if errors.Is(err, cloudconfig.ErrNoAppliedGeneration) {
		return httpapi.TriggerResponse{}, unavailable(err)
	}
	if err != nil {
		return httpapi.TriggerResponse{}, httpapi.NewInterventionError(http.StatusUnprocessableEntity, "trigger_workflow_ineligible", err.Error(), nil)
	}
	workflowDigest, gooberDigest, err := s.Pin(ctx, generation, request.Gaggle, request.Workflow)
	if err != nil {
		return httpapi.TriggerResponse{}, unavailable(err)
	}
	now := s.now()
	raw, err := (startintent.Envelope{
		Kind:    startintent.Kind,
		Request: startRequest(request),
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
		// A concurrent first acceptance with its own deadline and pins: compare
		// the stored request, not the bytes.
		if response, found, replayErr := s.replay(ctx, request); found {
			return response, replayErr
		}
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
