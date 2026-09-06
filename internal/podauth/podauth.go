// Package podauth implements pod-to-daemon authentication for the daemon
// write API (distributed-state-and-coordination.md §7/§14): per-run bearer
// tokens minted by the daemon at dispatch, presented by stage pods, verified
// against a daemon-local registry.
//
// The §14 open point offered two candidates — projected service-account
// tokens verified via TokenReview, or per-run minted bearers. This package is
// the per-run bearer: the daemon is already the mint for stage-scoped
// credentials (DS9/DS10), so the run token is one more short-lived secret on
// the same trust path; verification needs no Kubernetes API server, so mode 2
// and hermetic tests exercise the identical code path as mode 3; and dispatch
// payloads already carry opaque references only (#2931), which is exactly
// what a minted token reference is. The seam stays swappable: the daemon
// depends on httpapi.Authenticator, and a TokenReview implementation can
// replace this one without touching a handler.
//
// The registry is in-memory and daemon-local, which is sound under DS1 (one
// daemon, one instance root): a daemon restart invalidates outstanding
// tokens, and in-flight stage attempts that lose their token fail their API
// calls closed and retry on the infra budget (DS7) with a re-mint at the next
// dispatch.
package podauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

// tokenPrefix routes bearer tokens to this authenticator: a pod token is
// never a JWT, and a JWT never carries this prefix, so the two authenticator
// kinds can share one Authorization header without ambiguity.
const tokenPrefix = "goobers-pod."

// DefaultTokenTTL bounds a minted token's life when the minter passes zero.
// Sized like a claim lease: comfortably above a long stage attempt, small
// enough that a leaked token dies on its own.
const DefaultTokenTTL = 4 * time.Hour

// Plane scopes narrow what a minted bearer may do (Goobers#3897). The pod
// token itself is minted UNSCOPED and reaches every pod plane, because
// __dispatch-exec is the pod's own runtime: it surrenders the attempt,
// resolves credentials and moves blobs. A stage SUBPROCESS is a different
// principal in every way that matters — it is workflow-authored content
// running under a pinned command — and the environment it reads its plane
// bearers out of is one it can print. So the dispatcher mints it a separate,
// scoped bearer per plane, and this is what makes those bearers weaker than
// the pod token rather than merely differently spelled: a claims bearer
// presented to the surrender route is refused by the authorizer, not by the
// handler's own containment check.
//
// The scope names are wire values (they travel inside a signed token), so
// they are frozen: renaming one invalidates every outstanding token minted
// by a peer process that has not been redeployed.
const (
	// ScopeClaims admits the claims plane (acquire/renew/release/settle/list).
	ScopeClaims = "claims"
	// ScopeState admits the gaggle-scoped scheduler-state route and the
	// priority-trigger ingest that rides the same bearer.
	ScopeState = "state"
	// ScopeJournal admits the run-scoped journal routes: the three own-run
	// reads, the three gaggle-scoped cross-run questions, and emit.
	ScopeJournal = "journal"
	// ScopeTelemetry admits the ruled telemetry read routes.
	ScopeTelemetry = "telemetry"
	// ScopeSurrender admits the surrender route — deliberately NEVER minted
	// into a stage subprocess's environment. Named so a token that does hold
	// it is explicit rather than implied by an empty scope set.
	ScopeSurrender = "surrender"
	// ScopeBlob admits the blob plane.
	ScopeBlob = "blob"
	// ScopeCredential admits the credential-resolve route.
	ScopeCredential = "credential"
	// ScopeConfigDigest admits the config-digest read: which config tree the
	// daemon currently has in force. A worker polls it to notice that its own
	// tree has diverged, instead of discovering it when an agentic gate
	// refuses gate_pin_missing (#4153).
	ScopeConfigDigest = "config-digest"
)

// KnownScopes is every scope this package mints or verifies, in a stable
// order. A scope outside this set is refused at mint: a typo must not become
// a token that authorizes nothing and fails at the far side.
var KnownScopes = []string{
	ScopeClaims, ScopeState, ScopeJournal, ScopeTelemetry,
	ScopeSurrender, ScopeBlob, ScopeCredential, ScopeConfigDigest,
}

// ErrUnknownScope reports a mint request naming a scope outside KnownScopes.
var ErrUnknownScope = errors.New("podauth: unknown plane scope")

// checkScopes normalizes and validates a scope set. An EMPTY set is the
// unscoped pod token and is returned as nil — httpapi reads a principal with
// no scopes as the full pod-plane set, which is what GOOBERS_POD_TOKEN has
// always been.
func checkScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return nil, fmt.Errorf("%w: %q", ErrUnknownScope, scope)
		}
		if !slices.Contains(KnownScopes, scope) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownScope, scope)
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		out = append(out, scope)
	}
	// Stable order so a signed token's payload is a deterministic function of
	// its inputs — two mints of the same grant produce the same scope segment.
	sort.Strings(out)
	return out, nil
}

// ErrUnknownToken reports a pod-prefixed bearer the registry does not hold —
// never minted, expired, revoked, or minted by a previous daemon process.
var ErrUnknownToken = errors.New("podauth: unknown or expired pod token")

type grant struct {
	runID     string
	scopes    []string
	expiresAt time.Time
}

// Registry mints and verifies per-run pod tokens. Safe for concurrent use.
type Registry struct {
	mu  sync.Mutex
	now func() time.Time
	// grants is keyed by sha256(token) so the registry never stores a
	// credential it could leak; presented tokens are hashed and looked up.
	grants map[[sha256.Size]byte]grant
}

// NewRegistry constructs an empty token registry.
func NewRegistry() *Registry {
	return &Registry{now: time.Now, grants: make(map[[sha256.Size]byte]grant)}
}

// WithClock overrides the registry's time source for deterministic tests.
func (r *Registry) WithClock(now func() time.Time) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
	return r
}

// Mint issues a bearer token authenticating runID's stage pods until ttl
// elapses (DefaultTokenTTL when zero). The token value is returned exactly
// once — the registry retains only its hash — and travels in dispatch
// payloads as the opaque reference #2931 requires.
//
// The token is UNSCOPED: it reaches every pod plane, which is what the pod's
// own runtime needs. MintScoped is the least-privilege form handed to a stage
// subprocess.
func (r *Registry) Mint(runID string, ttl time.Duration) (string, error) {
	return r.MintScoped(runID, ttl)
}

// MintScoped issues a bearer confined to scopes (see KnownScopes). No scopes
// means the unscoped pod token — the same value Mint returns.
func (r *Registry) MintScoped(runID string, ttl time.Duration, scopes ...string) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", errors.New("podauth: run ID is required")
	}
	if ttl < 0 {
		return "", fmt.Errorf("podauth: token TTL must not be negative, got %s", ttl)
	}
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	checked, err := checkScopes(scopes)
	if err != nil {
		return "", err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("podauth: mint token: %w", err)
	}
	token := tokenPrefix + hex.EncodeToString(secret)

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	r.grants[sha256.Sum256([]byte(token))] = grant{runID: runID, scopes: checked, expiresAt: now.Add(ttl)}
	return token, nil
}

// Revoke invalidates every outstanding token for runID (run reached a
// terminal state, or its claims were force-released).
func (r *Registry) Revoke(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, g := range r.grants {
		if g.runID == runID {
			delete(r.grants, key)
		}
	}
}

// verify resolves a presented token to its run ID and scopes.
func (r *Registry) verify(token string) (string, []string, error) {
	digest := sha256.Sum256([]byte(token))
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	g, ok := r.grants[digest]
	if !ok || !g.expiresAt.After(now) {
		return "", nil, ErrUnknownToken
	}
	return g.runID, slices.Clone(g.scopes), nil
}

// pruneLocked drops expired grants so the registry stays bounded by live
// runs rather than by history. Caller holds r.mu.
func (r *Registry) pruneLocked(now time.Time) {
	for key, g := range r.grants {
		if !g.expiresAt.After(now) {
			delete(r.grants, key)
		}
	}
}

// Authenticator implements httpapi.Authenticator over a Registry, delegating
// requests that do not present a pod token to Fallback (the human
// authenticator — oidcauth in the shipped wiring). A request that does
// present a pod token never falls through: an invalid pod token fails
// closed rather than being retried as a JWT.
type Authenticator struct {
	verifier Verifier
	fallback httpapi.Authenticator
}

// Verifier resolves a presented pod token to the run it authenticates and the
// plane scopes it carries (nil = the unscoped pod token, which reaches every
// pod plane). The two implementations differ only in where the trust lives:
// Registry keeps grants in daemon memory (sound when the dispatcher shares
// that process), SignedKey verifies a shared-key signature (required when it
// does not — Goobers#3701).
type Verifier interface {
	verifyToken(token string) (runID string, scopes []string, err error)
}

func (r *Registry) verifyToken(token string) (string, []string, error)  { return r.verify(token) }
func (s *SignedKey) verifyToken(token string) (string, []string, error) { return s.verifySigned(token) }

// NewAuthenticator chains pod-token verification in front of fallback.
func NewAuthenticator(verifier Verifier, fallback httpapi.Authenticator) (*Authenticator, error) {
	if verifier == nil {
		return nil, errors.New("podauth: verifier is required")
	}
	if fallback == nil {
		// Deliberately still required. A pod-only daemon passes DenyAll
		// rather than nil, so "no human authenticator" is an explicit choice
		// at the call site instead of an omission that silently admits
		// unauthenticated requests.
		return nil, errors.New("podauth: fallback authenticator is required (use httpapi.DenyAllAuthenticator for a pod-only daemon)")
	}
	return &Authenticator{verifier: verifier, fallback: fallback}, nil
}

// Authenticate verifies a pod bearer token, or delegates to the fallback
// authenticator when the request carries none.
func (a *Authenticator) Authenticate(request *http.Request) (*httpapi.Principal, error) {
	token := bearerToken(request)
	if !isPodToken(token) {
		return a.fallback.Authenticate(request)
	}
	runID, scopes, err := a.verifier.verifyToken(token)
	if err != nil {
		return nil, err
	}
	return &httpapi.Principal{
		Subject: "run:" + runID,
		Issuer:  httpapi.PodPrincipalIssuer,
		Scopes:  scopes,
	}, nil
}

func isPodToken(token string) bool {
	// Constant-time on the prefix out of habit rather than need — the prefix
	// is public routing, not a secret.
	if len(token) < len(tokenPrefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token[:len(tokenPrefix)]), []byte(tokenPrefix)) == 1
}

func bearerToken(request *http.Request) string {
	authorization := request.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(authorization[len(prefix):])
}
