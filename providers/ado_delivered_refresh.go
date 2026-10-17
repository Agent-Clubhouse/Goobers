package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrInvalidADOCredentialDelivery marks a malformed broker response. It cannot
// be retried using an older cached credential, even if that value is unexpired.
var ErrInvalidADOCredentialDelivery = errors.New("invalid ADO credential delivery")

// NewADORefreshingCredentialSource holds a stage's delivered credential as one
// value/kind/expiry snapshot. resolve must use the same authority and capability
// as the initial delivery. The kind is pinned for the attempt: changing from
// Bearer to Basic requires a new attempt, never reinterpretation of a secret.
//
// Temporary refresh failures may retain a still-valid snapshot. An error with
// Deterministic() == true, a malformed snapshot, or a changed kind permanently
// refuses the attempt. Every accepted secret's wire forms are registered before
// use. The ADO transport owns the single invalidate/resend after HTTP 401.
func NewADORefreshingCredentialSource(label string, initial ADOCredential, resolve func(context.Context) (ADOCredential, error), registrar SecretRegistrar) (ADOCredentialSource, error) {
	if err := validateDeliveredADOCredential(initial); err != nil {
		return nil, err
	}
	if resolve == nil {
		return nil, errors.New("ADO delivered credential requires a resolver")
	}
	s := &adoRefreshingCredentialSource{label: label, current: initial, resolve: resolve, registrar: registrar, now: time.Now}
	s.register(initial)
	return s, nil
}

type adoRefreshingCredentialSource struct {
	mu          sync.Mutex
	label       string
	current     ADOCredential
	resolve     func(context.Context) (ADOCredential, error)
	registrar   SecretRegistrar
	now         func() time.Time
	invalid     bool
	lastRefresh time.Time
	terminal    error
}

func (s *adoRefreshingCredentialSource) Credential(ctx context.Context) (ADOCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ADOCredential{}, err
	}
	if s.terminal != nil {
		return ADOCredential{}, s.terminal
	}
	expired := !s.current.ExpiresAt.IsZero() && !s.now().Before(s.current.ExpiresAt)
	nearExpiry := !s.current.ExpiresAt.IsZero() && !s.now().Add(adoTokenRefreshSkew).Before(s.current.ExpiresAt)
	if !s.invalid && !expired && (!nearExpiry || !s.lastRefresh.IsZero() && s.now().Sub(s.lastRefresh) < time.Minute) {
		return s.current, nil
	}
	s.lastRefresh = s.now()
	refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	next, err := s.resolve(refreshCtx)
	cancel()
	if ctx.Err() != nil {
		return ADOCredential{}, ctx.Err()
	}
	var refusal interface{ Deterministic() bool }
	permanent := errors.Is(err, ErrInvalidADOCredentialDelivery) || errors.As(err, &refusal) && refusal.Deterministic()
	if err == nil {
		err = validateDeliveredADOCredential(next)
		if err == nil && (next.Kind != s.current.Kind || next.Username != s.current.Username) {
			err = errors.New("ADO delivered credential authorization scheme changed; start a new attempt")
		}
		if err == nil && !next.ExpiresAt.IsZero() && !s.now().Before(next.ExpiresAt) {
			err = errors.New("ADO credential resolver returned an expired value")
		}
		permanent = err != nil
	}
	if err != nil {
		if permanent {
			s.terminal = fmt.Errorf("%w for %s: %w", ErrADODeliveredCredentialRejected, s.label, err)
			return ADOCredential{}, s.terminal
		}
		if s.invalid || !s.current.ExpiresAt.IsZero() && !s.now().Before(s.current.ExpiresAt) {
			return ADOCredential{}, fmt.Errorf("%w for %s: no valid credential after refresh: %w", ErrADODeliveredCredentialRejected, s.label, err)
		}
		return s.current, nil
	}
	s.register(next)
	s.current, s.invalid = next, false
	return s.current, nil
}

func validateDeliveredADOCredential(c ADOCredential) error {
	if c.Kind != ADOCredentialKindPAT && c.Kind != ADOCredentialKindBearer {
		return errors.New("ADO delivered credential requires an explicit Basic or Bearer scheme")
	}
	if strings.TrimSpace(c.Secret) == "" {
		return errors.New("ADO delivered credential has no value")
	}
	return nil
}

func (s *adoRefreshingCredentialSource) register(c ADOCredential) {
	if s.registrar != nil {
		for _, form := range c.ScrubForms() {
			s.registrar.Register([]byte(form))
		}
	}
}

func (s *adoRefreshingCredentialSource) Invalidate() {
	s.mu.Lock()
	s.invalid = true
	s.mu.Unlock()
}

func (s *adoRefreshingCredentialSource) deliveredLabel() string { return s.label }

func (s *adoRefreshingCredentialSource) deliveredExpiry() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current.ExpiresAt
}
