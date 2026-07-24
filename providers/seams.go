package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// These seams let a provider report facts to higher layers (the run journal and
// telemetry) and resolve credentials, WITHOUT the providers package importing
// those layers. The run journal (issue #8) and the token-source seam (issue #14)
// are still under construction; a provider depends only on these small interfaces,
// and the runner adapts them to the concrete journal event and telemetry span when
// they land. All are optional: a nil implementation is a no-op.

// TokenSource resolves a provider access token at call time. It is the provider's
// view of the token-source seam (issue #14): credentials are injected, never
// hardcoded, and may be resolved (and refreshed) per request. When no TokenSource
// is configured the provider falls back to its statically injected token string.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// MutationRecorder records "external ref touched" facts (ARCHITECTURE.md §4) so a
// run journal can make every provider-side mutation traceable. The provider does
// not know the journal's on-disk shape; it reports the logical mutation and the
// runner projects it into a journal event.
type MutationRecorder interface {
	RecordExternalRef(ctx context.Context, ref ExternalRef)
}

// RateLimitObserver receives provider rate-limit / backoff signals so the runner
// can surface them to telemetry (e.g. telemetry.Span.Event). Kept provider-local
// so the providers package stays free of an OpenTelemetry dependency.
type RateLimitObserver interface {
	ObserveRateLimit(ctx context.Context, ev RateLimitEvent)
}

// QuotaCacheHitHeader marks a response synthesized from the shared provider
// snapshot cache. It is internal to Goobers' HTTP client/provider seam.
const QuotaCacheHitHeader = "X-Goobers-Quota-Cache-Hit"

// QuotaObserver receives successful and rate-limited response quota headers so
// a scheduler can budget future provider work before issuing requests.
type QuotaObserver interface {
	ObserveQuota(ctx context.Context, observation QuotaObservation)
}

// QuotaRequestGate reserves budget before a provider request is attempted.
type QuotaRequestGate interface {
	AcquireQuotaRequest(ctx context.Context, provider ProviderKind) error
}

// QuotaObservation describes one provider response. Known indicates an
// absolute remaining/reset window; Cached indicates no provider quota was used.
type QuotaObservation struct {
	Provider  ProviderKind
	Remaining int
	Reset     time.Time
	Known     bool
	Cached    bool
}

// RateLimitOutcome identifies the action taken after a rate-limit response.
type RateLimitOutcome string

// Rate-limit decision outcomes emitted to observers.
const (
	RateLimitOutcomeRetry     RateLimitOutcome = "retry"
	RateLimitOutcomeExhausted RateLimitOutcome = "exhausted"
	RateLimitOutcomeCanceled  RateLimitOutcome = "canceled"
)

// FieldDigest is the before/after content digest of a single mutated field. Empty
// Before means the field was newly created (e.g. a comment); empty After means it
// was cleared.
type FieldDigest struct {
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// ExternalRef is one "external ref touched" mutation: a change the provider made
// to an external system of record (a GitHub issue), described by content digests of
// the fields that changed so the journal stays tamper-evident without storing the
// raw values.
type ExternalRef struct {
	Provider  ProviderKind           `json:"provider"`
	Ref       string                 `json:"ref"`           // e.g. "owner/name#7"
	URL       string                 `json:"url,omitempty"` // canonical URL of the touched entity
	Operation string                 `json:"operation"`     // create|update|label|milestone|close|comment|claim|review|merge|delete
	Fields    map[string]FieldDigest `json:"fields,omitempty"`
	RunID     string                 `json:"runId,omitempty"` // set for claim mutations
}

// RateLimitEvent describes a single rate-limit backoff decision.
type RateLimitEvent struct {
	Provider   ProviderKind     `json:"provider"`
	Scope      string           `json:"scope"`
	Delay      time.Duration    `json:"delay"`
	Outcome    RateLimitOutcome `json:"outcome"`
	Endpoint   string           `json:"-"`
	Status     int              `json:"status"`
	Remaining  int              `json:"remaining"`
	Reset      time.Time        `json:"reset,omitempty"`
	RetryAfter time.Duration    `json:"retryAfter,omitempty"`
	Attempt    int              `json:"attempt"`
	Secondary  bool             `json:"secondary"` // GitHub secondary (abuse) rate limit
	// RetryAfterRaw/RemainingRaw/ResetRaw are the UNPARSED header string
	// values (e.g. "1", "0", "1784210000"), preserved alongside the parsed
	// Duration/int/time.Time fields above so a give-up RateLimitError's
	// Error() string still carries GitHub's own guidance verbatim — the
	// format IsTransientError's subprocess-crossed fallback classification
	// (hasRateLimitRetryGuidance, providers/transient.go) string-matches on,
	// once the typed error itself no longer survives a process boundary.
	RetryAfterRaw string `json:"-"`
	RemainingRaw  string `json:"-"`
	ResetRaw      string `json:"-"`
}

// digestString returns a stable, prefixed content digest of a field value. It is
// used for the before/after digests recorded on external-ref mutations.
func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
