package interactivesession

import (
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/sessioning"
)

func apiError(err error) error {
	if err == nil {
		return nil
	}
	var already *httpapi.InterventionError
	if errors.As(err, &already) {
		return err
	}
	err = mapped(err)
	for _, item := range []struct {
		match         error
		status        int
		code, message string
	}{
		{sessioning.ErrDenied, http.StatusForbidden, "session_forbidden", "This session operation is not authorized."},
		{sessioning.ErrNotFound, http.StatusNotFound, "session_not_found", "The session was not found."},
		{sessioning.ErrConflict, http.StatusConflict, "session_request_conflict", "The request key belongs to different input."},
		{sessioning.ErrCapacity, http.StatusTooManyRequests, "session_capacity", "Session capacity is exhausted. Retry with the same key."},
		{sessioning.ErrClosed, http.StatusConflict, "session_closed", "The session is closed to new input."},
		{sessioning.ErrExpired, http.StatusGone, "session_request_expired", "The request receipt expired. Create explicit new intent."},
		{sessioning.ErrInvalidRequest, http.StatusBadRequest, "invalid_session_request", "The session request is invalid."},
	} {
		if errors.Is(err, item.match) {
			return httpapi.NewInterventionError(item.status, item.code, item.message, err)
		}
	}
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, "session_unavailable", "The session operation is unavailable. Retry with the same key.", err)
}

func (s *Service) finishAcceptance(result sessioning.Acceptance, err error) (sessioning.Acceptance, error) {
	if err != nil {
		return sessioning.Acceptance{}, apiError(err)
	}
	result.Session.Title, err = s.clean(result.Session.Title, sessioning.MaxTitleBytes, false)
	if err == nil && result.Message != nil {
		result.Message.Text, err = s.clean(result.Message.Text, sessioning.MaxTextBytes, false)
		if !s.safeRepairTarget(result.Message.RepairTarget) {
			result.Message.RepairTarget = nil
		}
	}
	if err != nil {
		return sessioning.Acceptance{}, apiError(err)
	}
	return result, nil
}
