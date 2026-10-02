package credentials

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// RefreshWindow is how close to its stated expiry a delivered value is
// refreshed proactively (Goobers#6120). It matches the daemon-side sources'
// own refresh skew (githubapp's refreshSkew, the ADO bearer cache's
// adoTokenRefreshSkew), so a stage asks for a new value at the point the
// daemon can actually produce one.
const RefreshWindow = 5 * time.Minute

// proactiveRetryInterval bounds how often a value inside RefreshWindow is
// re-requested when the previous proactive attempt failed or returned a
// value that expires no later (the Azure CLI keeps returning its cached
// token until about five minutes before expiry). A 401 is never throttled
// here: it always gets its one re-resolve.
const proactiveRetryInterval = time.Minute

// proactiveRefreshTimeout bounds a proactive refresh: the caller still holds a
// valid value, so a slow control plane must not stall the request it serves.
const proactiveRefreshTimeout = 15 * time.Second

// ErrRejectedCredentialNotRefreshed reports that a value the provider
// rejected (Invalidate) could not be re-resolved: the request it served is an
// authentication failure, not a transport one.
var ErrRejectedCredentialNotRefreshed = errors.New("credentials: a rejected credential could not be re-resolved")

// RefreshFunc mints a fresh value and its stated expiry for one capability.
type RefreshFunc func(ctx context.Context) (token string, expiresAt time.Time, err error)

// RefreshingToken is a stage's delivered credential for one capability that
// can re-resolve itself (DS10: "the injected value is not final"). It serves
// the delivered value until one of two things happens:
//
//   - the value is within RefreshWindow of its stated expiry: Token
//     re-resolves first, and keeps serving the old value if that fails;
//   - Invalidate is called (the provider answered 401): the next Token
//     re-resolves and fails if it cannot, because the old value is known bad.
//
// Safe for concurrent use. Every refreshed value is passed to the registrar
// before it is returned, so it is scrubbed like the delivered one.
type RefreshingToken struct {
	capability string
	refresh    RefreshFunc
	registrar  SecretRegistrar
	now        func() time.Time

	mu            sync.Mutex
	token         string
	expiresAt     time.Time
	invalid       bool
	lastProactive time.Time
	refreshes     int
}

// NewRefreshingToken wraps a delivered value. expiresAt must be non-zero: a
// value that states no expiry (a PAT) is never refreshed, so callers keep the
// static value for it instead.
func NewRefreshingToken(capability, token string, expiresAt time.Time, refresh RefreshFunc, registrar SecretRegistrar) (*RefreshingToken, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("credentials: refreshing token for %s has no delivered value", capability)
	}
	if expiresAt.IsZero() {
		return nil, fmt.Errorf("credentials: refreshing token for %s has no stated expiry", capability)
	}
	if refresh == nil {
		return nil, errors.New("credentials: refreshing token requires a refresh function")
	}
	return &RefreshingToken{
		capability: capability, refresh: refresh, registrar: registrar, now: time.Now,
		token: token, expiresAt: expiresAt,
	}, nil
}

// NewInvalidationRefreshingToken wraps a value that states no expiry (a PAT)
// for a caller that owns a bounded retry around a provider 401, such as
// ci-poll (#6154): it is never refreshed proactively, only after Invalidate,
// so a long poll does not re-resolve on every request.
func NewInvalidationRefreshingToken(capability, token string, refresh RefreshFunc, registrar SecretRegistrar) (*RefreshingToken, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("credentials: refreshing token for %s has no delivered value", capability)
	}
	if refresh == nil {
		return nil, errors.New("credentials: refreshing token requires a refresh function")
	}
	return &RefreshingToken{
		capability: capability, refresh: refresh, registrar: registrar, now: time.Now,
		token: token,
	}, nil
}

// WithClock overrides the time source for deterministic tests.
func (t *RefreshingToken) WithClock(now func() time.Time) *RefreshingToken {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = now
	return t
}

// Capability names the capability the value was delivered for.
func (t *RefreshingToken) Capability() string { return t.capability }

// Token returns the current value, re-resolving it first when it is inside
// the refresh window or has been invalidated.
func (t *RefreshingToken) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t.invalid {
		if err := t.refreshLocked(ctx); err != nil {
			return "", fmt.Errorf("%w (%s): %w", ErrRejectedCredentialNotRefreshed, t.capability, err)
		}
		return t.token, nil
	}
	if t.dueLocked() {
		t.lastProactive = t.now()
		refreshCtx, cancel := context.WithTimeout(ctx, proactiveRefreshTimeout)
		_ = t.refreshLocked(refreshCtx) // the current value is still valid; keep it on failure
		cancel()
	}
	return t.token, nil
}

// Expiry reports the stated expiry of the value Token last returned.
func (t *RefreshingToken) Expiry() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.expiresAt
}

// Refreshes reports how many re-resolves have succeeded.
func (t *RefreshingToken) Refreshes() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refreshes
}

// Invalidate marks the current value rejected: the next Token re-resolves.
func (t *RefreshingToken) Invalidate() {
	t.mu.Lock()
	t.invalid = true
	t.mu.Unlock()
}

func (t *RefreshingToken) dueLocked() bool {
	now := t.now()
	if t.expiresAt.IsZero() || now.Add(RefreshWindow).Before(t.expiresAt) {
		return false
	}
	return t.lastProactive.IsZero() || now.Sub(t.lastProactive) >= proactiveRetryInterval
}

func (t *RefreshingToken) refreshLocked(ctx context.Context) error {
	token, expiresAt, err := t.refresh(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("the credential plane returned an empty value")
	}
	if t.registrar != nil {
		t.registrar.Register([]byte(token))
	}
	t.token, t.expiresAt, t.invalid = token, expiresAt, false
	t.refreshes++
	return nil
}
