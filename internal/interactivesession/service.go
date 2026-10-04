// Package interactivesession owns durable shared human conversations. HTTP
// request lifetimes end at acceptance; only the host coordinator starts work.
package interactivesession

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ProfilePinner validates the selected configured Goober and returns immutable
// archive/content pins. It performs no provider or model execution.
type ProfilePinner func(context.Context, string, string) (sessioning.Profile, error)

// Service reuses applied human policy, the ordinary start database and shared
// redaction. Runtime is installed separately so schema availability never
// advertises an execution adapter that has not been configured.
type Service struct {
	Queue       *triggerqueue.Store
	Permissions *interactiveaccess.Service
	Scrubber    journal.Scrubber
	Pin         ProfilePinner
	Now         func() time.Time
	Runtime     *Runtime
	execution   executionState
}

func (s *Service) ready() error {
	if s == nil || s.Queue == nil || s.Permissions == nil || s.Scrubber == nil || s.Now == nil {
		return sessioning.ErrUnavailable
	}
	return nil
}

func command(p httpapi.Principal, gaggle, key string, request any) (triggerqueue.SessionCommand, error) {
	if p.Subject == "" || p.Issuer == "" || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) > 0 {
		return triggerqueue.SessionCommand{}, sessioning.ErrDenied
	}
	if key == "" || len(key) > 256 || strings.TrimSpace(key) != key {
		return triggerqueue.SessionCommand{}, sessioning.ErrInvalidRequest
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return triggerqueue.SessionCommand{}, err
	}
	return triggerqueue.SessionCommand{Gaggle: gaggle, Actor: sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}, RequestID: key, RequestDigest: journal.Digest(raw)}, nil
}

func mapped(err error) error {
	switch {
	case errors.Is(err, interactiveaccess.ErrDenied):
		return sessioning.ErrDenied
	case errors.Is(err, sql.ErrNoRows):
		return sessioning.ErrNotFound
	case errors.Is(err, triggerqueue.ErrConflict):
		return sessioning.ErrConflict
	case errors.Is(err, triggerqueue.ErrFull):
		return sessioning.ErrCapacity
	case errors.Is(err, triggerqueue.ErrSessionClosed):
		return sessioning.ErrClosed
	case errors.Is(err, triggerqueue.ErrSessionExpired):
		return sessioning.ErrExpired
	case errors.Is(err, triggerqueue.ErrTransition):
		return sessioning.ErrInvalidRequest
	default:
		return err
	}
}

func (s *Service) clean(text string, max int, required bool) (string, error) {
	if len(text) > max || !utf8.ValidString(text) || strings.ContainsRune(text, 0) || (required && strings.TrimSpace(text) == "") {
		return "", sessioning.ErrInvalidRequest
	}
	result := string(s.Scrubber.Scrub([]byte(text)))
	if len(result) > max || !utf8.ValidString(result) {
		return "", sessioning.ErrInvalidRequest
	}
	return result, nil
}

// Create opens an idle shared session with a selected existing Goober.
func (s *Service) Create(ctx context.Context, p httpapi.Principal, gaggle string, req sessioning.CreateRequest) (result sessioning.Acceptance, err error) {
	defer func() { result, err = s.finishAcceptance(result, err) }()
	if err := s.ready(); err != nil {
		return result, err
	}
	c, err := command(p, gaggle, req.RequestID, req)
	if err != nil {
		return result, err
	}
	req.Title, err = s.clean(req.Title, sessioning.MaxTitleBytes, true)
	if err != nil {
		return result, err
	}
	if req.Goober == "" || len(req.Goober) > 128 || strings.TrimSpace(req.Goober) != req.Goober {
		return result, sessioning.ErrInvalidRequest
	}
	err = s.Permissions.WithAuthorization(ctx, p, gaggle, "session.create", func(ctx context.Context) error {
		var found bool
		result, found, err = s.Queue.SessionReplay(ctx, c, "create", "")
		if err != nil || found {
			return err
		}
		if s.Pin == nil {
			return sessioning.ErrUnavailable
		}
		profile, pinErr := s.Pin(ctx, gaggle, req.Goober)
		if pinErr != nil {
			return pinErr
		}
		if profile.Goober != req.Goober {
			return sessioning.ErrUnavailable
		}
		result, err = s.Queue.CreateSession(ctx, c, req.Title, profile, s.Now())
		return err
	})
	return result, mapped(err)
}

// SubmitMessage persists attribution and queues one turn under current policy.
func (s *Service) SubmitMessage(ctx context.Context, p httpapi.Principal, gaggle, id string, req sessioning.MessageRequest) (result sessioning.Acceptance, err error) {
	defer func() { result, err = s.finishAcceptance(result, err) }()
	if err := s.ready(); err != nil {
		return result, err
	}
	c, err := command(p, gaggle, req.RequestID, req)
	if err != nil {
		return result, err
	}
	req.Text, err = s.clean(req.Text, sessioning.MaxTextBytes, true)
	if err != nil {
		return result, err
	}
	authority, err := marshalAuthority(p)
	if err != nil {
		return result, err
	}
	err = s.Permissions.WithAuthorization(ctx, p, gaggle, "session.message", func(ctx context.Context) error {
		result, err = s.Queue.SubmitSessionMessage(ctx, c, id, req.Text, authority, s.Now())
		return err
	})
	return result, mapped(err)
}

// Close records human intent. A runtime sweep cancels and joins any active turn.
func (s *Service) Close(ctx context.Context, p httpapi.Principal, gaggle, id string, req sessioning.CloseRequest) (result sessioning.Acceptance, err error) {
	defer func() { result, err = s.finishAcceptance(result, err) }()
	if err := s.ready(); err != nil {
		return result, err
	}
	c, err := command(p, gaggle, req.RequestID, req)
	if err != nil {
		return result, err
	}
	req.Reason, err = s.clean(req.Reason, 4096, false)
	if err != nil {
		return result, err
	}
	err = s.Permissions.WithAuthorization(ctx, p, gaggle, "session.message", func(ctx context.Context) error {
		result, err = s.Queue.CloseSession(ctx, c, id, req.Reason, s.Now())
		return err
	})
	return result, mapped(err)
}
