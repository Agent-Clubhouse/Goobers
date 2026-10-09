package localscheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// ReasonProviderAuthUnhealthy prefixes a tick.skipped reason when a workflow
// that sets readiness.requireProviderAuthorization is refused autonomous
// dispatch because its provider credential failed the pre-claim health
// check (#5317). It is distinct from ReasonProviderAuth, which is the
// reactive circuit opened after a run already failed on credentials.
const ReasonProviderAuthUnhealthy = "conditions: provider-auth-unhealthy"

// Stable provider authorization health codes (#5317). They appear in the
// tick.skipped reason and ErrorDetail.Code and never carry token material.
const (
	// ProviderAuthMissing: the instance could not resolve a credential.
	ProviderAuthMissing = "provider_auth_missing"
	// ProviderAuthRejected: the provider rejected the credential (HTTP 401:
	// invalid, expired, or revoked).
	ProviderAuthRejected = "provider_auth_rejected"
	// ProviderAuthInsufficient: the credential authenticated but cannot read
	// the repository or its issues.
	ProviderAuthInsufficient = "provider_auth_insufficient"
	// ProviderAuthUnverified: the check could not complete (network, 5xx,
	// rate limit, timeout). Health is unknown, so dispatch fails closed.
	ProviderAuthUnverified = "provider_auth_unverified"
	// ProviderAuthUnsupported: no health check exists for this provider.
	ProviderAuthUnsupported = "provider_auth_unsupported"
)

const (
	providerAuthHealthyTTL    = 5 * time.Minute
	providerAuthUnhealthyTTL  = time.Minute
	providerAuthUnverifiedTTL = 30 * time.Second
	providerAuthCheckTimeout  = 15 * time.Second
	providerAuthCacheLimit    = 256
	providerAuthDetailLimit   = 240
)

// ProviderAuthStatus is the outcome of one provider authorization health
// evaluation for a workflow's repository.
type ProviderAuthStatus struct {
	Healthy    bool
	Code       string
	Detail     string
	Provider   providers.ProviderKind
	Repository string
	CheckedAt  time.Time
}

// ProviderAuthGate evaluates provider authorization health before a workflow
// may be autonomously dispatched. Implementations must not mutate provider
// state.
type ProviderAuthGate interface {
	ProviderAuthStatus(ctx context.Context, now time.Time) ProviderAuthStatus
}

// ProviderAuthTarget names the repository and credential a workflow's
// autonomous runs would use.
type ProviderAuthTarget struct {
	Repository    providers.RepositoryRef
	CredentialRef string
}

// ProviderAuthVerifier performs the non-mutating provider read with token.
type ProviderAuthVerifier func(ctx context.Context, token string, repo providers.RepositoryRef) error

// ProviderAuthHealth builds per-workflow gates sharing one bounded health
// cache. Entries are keyed by provider, repository, credential ref, credential
// fingerprint, and config revision, so a rotated credential or a reloaded
// configuration never reuses earlier evidence.
type ProviderAuthHealth struct {
	revision string
	resolve  func(ctx context.Context, ref string) (string, error)
	register func([]byte)
	verify   ProviderAuthVerifier

	mu      sync.Mutex
	entries map[providerAuthKey]providerAuthCacheEntry
}

type providerAuthKey struct {
	provider      providers.ProviderKind
	repository    string
	credentialRef string
	fingerprint   string
	revision      string
}

type providerAuthCacheEntry struct {
	status    ProviderAuthStatus
	expiresAt time.Time
}

// NewProviderAuthHealth returns a health cache for config revision. resolve is
// the instance's credential resolver, register scrubs each resolved token from
// logs, and verify performs the GitHub read probe.
func NewProviderAuthHealth(revision string, resolve func(ctx context.Context, ref string) (string, error), register func([]byte), verify ProviderAuthVerifier) *ProviderAuthHealth {
	return &ProviderAuthHealth{
		revision: revision,
		resolve:  resolve,
		register: register,
		verify:   verify,
		entries:  map[providerAuthKey]providerAuthCacheEntry{},
	}
}

// Gate returns the health gate for one workflow target.
func (h *ProviderAuthHealth) Gate(target ProviderAuthTarget) ProviderAuthGate {
	return providerAuthTargetGate{health: h, target: target}
}

type providerAuthTargetGate struct {
	health *ProviderAuthHealth
	target ProviderAuthTarget
}

func (g providerAuthTargetGate) ProviderAuthStatus(ctx context.Context, now time.Time) ProviderAuthStatus {
	return g.health.status(ctx, g.target, now)
}

func (h *ProviderAuthHealth) status(ctx context.Context, target ProviderAuthTarget, now time.Time) ProviderAuthStatus {
	repo := target.Repository
	base := ProviderAuthStatus{Provider: repo.Provider, Repository: repo.Owner + "/" + repo.Name, CheckedAt: now}
	if repo.Provider != providers.ProviderGitHub || h.resolve == nil || h.verify == nil {
		return base.unhealthy(ProviderAuthUnsupported, "no authorization health check for provider "+string(repo.Provider))
	}
	token, err := h.resolve(ctx, target.CredentialRef)
	if err != nil || token == "" {
		return base.unhealthy(ProviderAuthMissing, "credential "+target.CredentialRef+" did not resolve")
	}
	if h.register != nil {
		h.register([]byte(token))
	}
	key := providerAuthKey{
		provider: repo.Provider, repository: base.Repository, credentialRef: target.CredentialRef,
		fingerprint: credentialFingerprint(token), revision: h.revision,
	}
	if cached, ok := h.cached(key, now); ok {
		return cached
	}
	checkCtx, cancel := context.WithTimeout(ctx, providerAuthCheckTimeout)
	verifyErr := h.verify(checkCtx, token, repo)
	cancel()
	status := base
	status.Healthy = verifyErr == nil
	ttl := providerAuthHealthyTTL
	if verifyErr != nil {
		status.Code = classifyProviderAuthError(verifyErr)
		status.Detail = truncateProviderAuthDetail(verifyErr.Error())
		ttl = providerAuthUnhealthyTTL
		if status.Code == ProviderAuthUnverified {
			ttl = providerAuthUnverifiedTTL
		}
	}
	if ctx.Err() == nil {
		h.store(key, status, now.Add(ttl))
	}
	return status
}

func (s ProviderAuthStatus) unhealthy(code, detail string) ProviderAuthStatus {
	s.Code, s.Detail = code, detail
	return s
}

func (h *ProviderAuthHealth) cached(key providerAuthKey, now time.Time) (ProviderAuthStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		return ProviderAuthStatus{}, false
	}
	return entry.status, true
}

func (h *ProviderAuthHealth) store(key providerAuthKey, status ProviderAuthStatus, expiresAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.entries[key]; !exists && len(h.entries) >= providerAuthCacheLimit {
		h.evictLocked(status.CheckedAt)
	}
	h.entries[key] = providerAuthCacheEntry{status: status, expiresAt: expiresAt}
}

// evictLocked drops expired entries, then the soonest-expiring one if the
// cache is still full, keeping memory bounded.
func (h *ProviderAuthHealth) evictLocked(now time.Time) {
	var oldest providerAuthKey
	var oldestAt time.Time
	for key, entry := range h.entries {
		if !now.Before(entry.expiresAt) {
			delete(h.entries, key)
			continue
		}
		if oldestAt.IsZero() || entry.expiresAt.Before(oldestAt) {
			oldest, oldestAt = key, entry.expiresAt
		}
	}
	if len(h.entries) >= providerAuthCacheLimit {
		delete(h.entries, oldest)
	}
}

func credentialFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

func classifyProviderAuthError(err error) string {
	switch {
	case providers.IsUnauthorizedError(err):
		return ProviderAuthRejected
	case errors.Is(err, providers.ErrInsufficientRepositoryPermission),
		providers.IsNotFoundError(err),
		providers.IsAuthenticationError(err):
		return ProviderAuthInsufficient
	default:
		return ProviderAuthUnverified
	}
}

func truncateProviderAuthDetail(detail string) string {
	if len(detail) <= providerAuthDetailLimit {
		return detail
	}
	return detail[:providerAuthDetailLimit] + "..."
}

// providerAuthDispatchRefusal enforces readiness.requireProviderAuthorization
// before any slot is reserved or work claimed. Manual triggers bypass it so an
// operator can still run a workflow to diagnose the credential. A missing gate
// fails closed.
func (s *Scheduler) providerAuthDispatchRefusal(ctx context.Context, entry WorkflowEntry, identity WorkflowIdentity, trigger journal.Trigger, now time.Time, triggerReason string) (string, bool) {
	if !entry.Readiness.RequireProviderAuthorization || trigger.Kind == journal.TriggerManual {
		return "", false
	}
	status := ProviderAuthStatus{Code: ProviderAuthUnsupported, Detail: "no provider authorization health gate configured"}
	if entry.ProviderAuth != nil {
		status = entry.ProviderAuth.ProviderAuthStatus(ctx, now)
	}
	if status.Healthy {
		return "", false
	}
	reason := ReasonProviderAuthUnhealthy + ": " + status.Code + ": " + status.Detail
	s.journalEvent(journal.Event{
		Type:     journal.EventTickSkipped,
		Workflow: entry.Workflow,
		Gaggle:   entry.Gaggle,
		Reason:   s.refillRejectionReason(identity, now, triggerReason, reason),
		Error:    &journal.ErrorDetail{Code: status.Code, Message: status.Detail},
	})
	return reason, true
}

var _ = telemetry.OutcomeBlocked
