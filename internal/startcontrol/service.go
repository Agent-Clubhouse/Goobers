package startcontrol

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Authorization rechecks current explicit human gaggle access for every read or
// command. The callbacks may only complete bounded work under that policy lease.
type Authorization interface {
	WithSourceView(context.Context, httpapi.Principal, string, func(context.Context, *apiv1.Gaggle) error) error
	WithQueueCancellation(context.Context, httpapi.Principal, string, func(context.Context) error) error
}

// Service exposes control receipts without exposing source payloads or retained
// principal claims. Stop is a qualified host adapter, never a caller-provided URL.
type Service struct {
	Controls *Coordinator
	Access   Authorization
	Scrubber journal.Scrubber
	Stop     func(context.Context, triggerqueue.StartControl) (CancellationObservation, error)
}

func queueError(err error) error {
	switch {
	case errors.Is(err, interactiveaccess.ErrDenied):
		return httpapi.NewInterventionError(http.StatusForbidden, "queue_access_denied", "Start queue access is not authorized.", nil)
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, ErrLegacyUnscoped):
		return httpapi.NewInterventionError(http.StatusNotFound, "queued_start_not_found", "Scoped start receipt not found.", nil)
	case errors.Is(err, triggerqueue.ErrConflict):
		return httpapi.NewInterventionError(http.StatusConflict, "queue_request_conflict", "The request differs from retained custody; use its original command or a new request.", nil)
	case errors.Is(err, triggerqueue.ErrTransition):
		return httpapi.NewInterventionError(http.StatusConflict, "queue_state_changed", "The start has already settled or its custody changed. Refresh the receipt.", nil)
	case errors.Is(err, triggerqueue.ErrTypedStartSettlement):
		return httpapi.NewInterventionError(http.StatusServiceUnavailable, "queue_source_control_unavailable", "The source cancellation adapter is unavailable; custody is unchanged.", nil)
	default:
		return httpapi.NewInterventionError(http.StatusServiceUnavailable, "queue_unavailable", "Start queue custody is temporarily unavailable. Retry the same command.", nil)
	}
}
func (s *Service) ready() bool {
	return s != nil && s.Controls != nil && s.Controls.Queue != nil && s.Controls.Now != nil && s.Access != nil && s.Scrubber != nil
}

// StartQueue returns one authorized explicit window, never an inferred count.
func (s *Service) StartQueue(ctx context.Context, p httpapi.Principal, gaggle, cursor string, limit int) (apicontract.StartQueuePage, error) {
	result := apicontract.StartQueuePage{Gaggle: gaggle, Items: []apicontract.StartQueueItem{}}
	if !s.ready() {
		return result, queueError(errors.New("queue unavailable"))
	}
	if limit < 1 || limit > 50 || len(cursor) > 128 {
		return result, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, "Invalid bounded queue window.", nil)
	}
	err := s.Access.WithSourceView(ctx, p, gaggle, func(ctx context.Context, _ *apiv1.Gaggle) error {
		page, err := s.Controls.Queue.StartControlPage(ctx, gaggle, cursor, limit+1)
		if err != nil {
			return err
		}
		if len(page) > limit {
			result.NextCursor = page[limit-1].Record.ID
			page = page[:limit]
		}
		for _, control := range page {
			result.Items = append(result.Items, s.view(control))
		}
		return nil
	})
	if err != nil {
		return apicontract.StartQueuePage{}, queueError(err)
	}
	return result, nil
}

// StartQueueItem reveals receipt state only after current gaggle visibility.
func (s *Service) StartQueueItem(ctx context.Context, p httpapi.Principal, gaggle, id string) (apicontract.StartQueueItem, error) {
	var result apicontract.StartQueueItem
	if !s.ready() {
		return result, queueError(errors.New("queue unavailable"))
	}
	err := s.Access.WithSourceView(ctx, p, gaggle, func(ctx context.Context, _ *apiv1.Gaggle) error {
		c, err := s.Controls.Queue.StartControl(ctx, gaggle, id)
		if err == nil {
			result = s.view(c)
		}
		return err
	})
	if err != nil {
		return result, queueError(err)
	}
	return result, nil
}

// CancelQueuedStart accepts an idempotent attributed command under current
// policy. Attempted execution is cancelled by later qualified host maintenance.
func (s *Service) CancelQueuedStart(ctx context.Context, p httpapi.Principal, gaggle, id string, input apicontract.StartQueueCancelInput) (apicontract.StartQueueItem, error) {
	var result apicontract.StartQueueItem
	if !s.ready() {
		return result, queueError(errors.New("queue unavailable"))
	}
	if !commandText(input.RequestID, 128) || !commandText(input.Reason, 512) || string(s.Scrubber.Scrub([]byte(input.Reason))) != input.Reason {
		return result, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, "A bounded request ID and persistable reason are required.", nil)
	}
	err := s.Access.WithQueueCancellation(ctx, p, gaggle, func(ctx context.Context) error {
		control, err := s.Controls.Queue.StartControl(ctx, gaggle, id)
		if err != nil {
			return err
		}
		command, err := cancellationCommand(p, control, input)
		if err != nil {
			return err
		}
		control, _, err = s.Controls.Queue.RequestStartCancellation(ctx, gaggle, id, command, s.Controls.Now())
		if err == nil {
			result = s.view(control)
		}
		return err
	})
	if err != nil {
		return result, queueError(err)
	}
	return result, nil
}
func (s *Service) view(c triggerqueue.StartControl) apicontract.StartQueueItem {
	item := apicontract.StartQueueItem{AcceptanceID: c.Record.ID, Gaggle: c.Scope.Gaggle, Workflow: c.Scope.Workflow, Source: c.Scope.Source, Generation: c.Scope.Generation, AcceptedAt: c.Record.AcceptedAt, State: string(c.Record.State), Disposition: c.Disposition, WaitingReason: waitingReason(c)}
	if !c.Scope.Deadline.IsZero() {
		deadline := c.Scope.Deadline
		item.Deadline = &deadline
	}
	if c.Record.State == triggerqueue.Dispatched {
		item.RunID = c.Record.RunID
	}
	if c.Cancellation != nil {
		state := "requested"
		if c.CancellationOutcome != "" {
			state = c.CancellationOutcome
		}
		if c.Disposition == "cancelled" {
			state = "cancelled-before-dispatch"
		}
		item.Cancellation = &apicontract.StartQueueCancellation{RequestID: c.Cancellation.RequestID, Actor: string(s.Scrubber.Scrub([]byte(c.Cancellation.Actor))), Reason: string(s.Scrubber.Scrub([]byte(c.Cancellation.Reason))), RequestedAt: c.CancelRequestedAt, State: state}
	}
	return item
}
func waitingReason(c triggerqueue.StartControl) string {
	if c.Cancellation != nil && c.Disposition == "" && c.CancellationOutcome == "" {
		return "Cancellation requested; execution termination is not yet confirmed."
	}
	if c.Record.State == triggerqueue.Dispatching {
		return "Waiting for execution confirmation; this request will not be resent."
	}
	if c.Record.State != triggerqueue.Accepted {
		return ""
	}
	for _, reason := range []triggerqueue.WaitingReason{triggerqueue.WaitingCapacity, triggerqueue.WaitingAccess, triggerqueue.WaitingSource, triggerqueue.WaitingSourceBusy, triggerqueue.WaitingUnsupported, triggerqueue.WaitingValidation} {
		if c.Record.Reason == reason.Message() {
			return c.Record.Reason
		}
	}
	return "Waiting for execution admission."
}
func commandText(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value && strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) < 0
}

// ReconcileCancellation rechecks current authority before each bounded host
// attempt. A failed/refused observation leaves both request and effect retained.
func (s *Service) ReconcileCancellation(ctx context.Context, c triggerqueue.StartControl) error {
	if c.Cancellation == nil || c.Disposition != "" || c.CancellationOutcome != "" || s.Stop == nil {
		return nil
	}
	a, err := readCancellationAuthority(c)
	if err != nil {
		return err
	}
	p := httpapi.Principal{Issuer: a.Issuer, Subject: a.Subject, Roles: a.Roles, Groups: a.Groups}
	return s.Access.WithQueueCancellation(ctx, p, c.Scope.Gaggle, func(ctx context.Context) error {
		observation, err := s.Stop(ctx, c)
		if err != nil {
			return err
		}
		switch observation.State {
		case CancellationRequested:
			return nil
		case CancellationConfirmed, CancellationAlreadyTerminal:
			_, err = s.Controls.Queue.CompleteStartCancellation(ctx, c.Scope.Gaggle, c.Record.ID, c.Cancellation.RequestID, string(observation.State), s.Controls.Now())
			return err
		default:
			return triggerqueue.ErrTransition
		}
	})
}
