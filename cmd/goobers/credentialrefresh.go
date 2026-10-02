package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/podauth"
)

// credentialrefresh.go is the daemon side of mid-stage credential refresh
// (Goobers#6120 phase 1; distributed-state-and-coordination.md DS10, §11,
// acceptance item 8).
//
// THREAT MODEL, in one place:
//
//   - WHO GETS A GRANT. Only a deterministic task's attempt: the local runner
//     mints one for a goobers-CLI stage whose delivered credentials state an
//     expiry (executor.appendCredentialGrant), and the credential plane mints
//     one for a pod's stage-start resolve when the pinned definition says the
//     stage is a deterministic task (mintPodStageGrant). Agentic stages and
//     reviewer gates never get one (phase 2), and the refresh route refuses
//     them even if a grant for one were forged into existence.
//   - WHAT IT MINTS. One capability per call, from the grant's own list —
//     the stage's declared capabilities whose delivered value expires — and
//     re-verified against the run's PINNED definition on every call, through
//     the same capability-gated injector the stage-start resolve uses.
//   - HOW LONG. The stage timeout plus a margin, capped at
//     podauth.MaxCredentialGrantTTL.
//   - HOW IT ENDS. It expires; a local grant is revoked when its attempt
//     returns; and every grant dies with the daemon process, because the
//     signing key is generated at startup and never persisted.
//   - HOW FAST. Each grant is rate-limited (grantRefreshBurst, then one per
//     grantRefreshInterval), and every refresh is journaled with the attempt.

// credentialRefreshMarker identifies a mid-stage re-resolve's audit record.
const credentialRefreshMarker = "credentials.refreshed"

// grantRefreshBurst and grantRefreshInterval bound how often one grant may
// mint. A stage refreshes a capability when it nears expiry or after a 401,
// so a handful of calls an hour is the expected rate; the burst leaves room
// for several capabilities expiring together.
const (
	grantRefreshBurst    = 6
	grantRefreshInterval = 20 * time.Second
)

// stageGrantIssuer holds the daemon's grant signing key and the per-grant
// state the refresh route needs. The key is random per daemon process.
type stageGrantIssuer struct {
	key      *podauth.SignedKey
	endpoint string
	now      func() time.Time

	mu sync.Mutex
	// revoked is keyed by grant ID, not by run/stage/attempt: a revisited
	// stage restarts its attempt numbering, so a later execution of the same
	// stage can carry the same triple as a finished one.
	revoked map[string]time.Time
	buckets map[[sha256.Size]byte]*grantBucket
}

type grantBucket struct {
	tokens    float64
	last      time.Time
	expiresAt time.Time
}

func newStageGrantIssuer(endpoint string) (*stageGrantIssuer, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate credential grant key: %w", err)
	}
	key, err := podauth.NewSignedKey(secret)
	if err != nil {
		return nil, err
	}
	return &stageGrantIssuer{
		key: key, endpoint: endpoint, now: time.Now,
		revoked: map[string]time.Time{},
		buckets: map[[sha256.Size]byte]*grantBucket{},
	}, nil
}

// withStageGrants enables mid-stage credential refresh on this daemon's
// credential plane, served at the API reachable at listenAddress, and offers
// the minter to the local runners of the instance at root. A daemon that
// cannot construct the issuer keeps serving with refresh disabled: stages
// then behave exactly as they did before grants existed.
func (s *daemonCredentialService) withStageGrants(root, listenAddress string, tls bool) *daemonCredentialService {
	endpoint, err := localCredentialEndpoint(listenAddress, tls)
	if err != nil {
		return s
	}
	issuer, err := newStageGrantIssuer(endpoint)
	if err != nil {
		return s
	}
	s.grants = issuer
	registerStageGrantMinter(root, s)
	return s
}

// grantKey is the verifier the API authenticator admits grants with; nil when
// refresh is disabled.
func (s *daemonCredentialService) grantKey() *podauth.SignedKey {
	if s == nil || s.grants == nil {
		return nil
	}
	return s.grants.key
}

// localCredentialEndpoint turns the bound API address into the URL a stage on
// this host dials: an unspecified bind address is reached over loopback.
func localCredentialEndpoint(listenAddress string, tls bool) (string, error) {
	host, port, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

// MintStageGrant implements executor.StageCredentialGrants for the local
// runner: a grant for exactly capabilities of env's attempt, revoked when the
// attempt returns.
//
// The grant names the bare stage, never env.TaskID. The local runner's
// envelope TaskID is the run-scoped "<runId>:<stage>" instance id, while the
// refresh route verifies grant.Stage against the run's PINNED definition by
// stage name, so stamping the composite id refused every local refresh with
// 404 stage_unknown (Goobers#6193). It is the same run qualifier #4119 strips
// from stage-artifact names, so the same helper strips it here. Only this
// run's own prefix is stripped: a TaskID of any other shape is carried
// verbatim and the pinned-definition check still refuses it.
func (s *daemonCredentialService) MintStageGrant(env apiv1.InvocationEnvelope, capabilities []string, ttl time.Duration) (executor.StageCredentialGrant, error) {
	if s.grants == nil {
		return executor.StageCredentialGrant{}, errors.New("credential refresh is not enabled on this daemon")
	}
	token, grant, err := s.grants.key.MintCredentialGrant(podauth.CredentialGrant{
		RunID: env.RunID, Stage: stageArtifactName(env.RunID, env.TaskID), Attempt: env.Attempt, Capabilities: capabilities,
	}, ttl)
	if err != nil {
		return executor.StageCredentialGrant{}, err
	}
	s.shared.RegisterUntil([]byte(token), grant.ExpiresAt)
	var once sync.Once
	return executor.StageCredentialGrant{
		Endpoint: s.grants.endpoint,
		Token:    token,
		Revoke:   func() { once.Do(func() { s.grants.revoke(grant.ID, grant.ExpiresAt) }) },
	}, nil
}

// mintPodStageGrant mints the grant a pod hands its deterministic stage
// child. Nil — no grant, the pre-grant behavior — when refresh is disabled,
// the stage is not a deterministic task, or none of its credentials expire.
func (s *daemonCredentialService) mintPodStageGrant(request httpapi.CredentialResolveRequest, resolved stageResolution) *httpapi.CredentialGrantDelivery {
	if s.grants == nil || !resolved.profile.deterministic {
		return nil
	}
	expiring := expiringDeclaredCapabilities(resolved)
	if len(expiring) == 0 {
		return nil
	}
	timeout := resolved.profile.timeout
	if timeout <= 0 {
		timeout = time.Duration(request.TimeoutSeconds) * time.Second
	}
	token, grant, err := s.grants.key.MintCredentialGrant(podauth.CredentialGrant{
		RunID: request.RunID, Stage: request.Stage, Attempt: request.Attempt, Capabilities: expiring,
	}, executor.CredentialGrantTTL(timeout))
	if err != nil {
		return nil
	}
	s.shared.RegisterUntil([]byte(token), grant.ExpiresAt)
	return &httpapi.CredentialGrantDelivery{Token: token, ExpiresAt: grant.ExpiresAt}
}

// expiringDeclaredCapabilities lists the stage's DECLARED capabilities whose
// minted value states an expiry. Implicit keys (the checkout capability) are
// never in a grant: the stage never receives them.
func expiringDeclaredCapabilities(resolved stageResolution) []string {
	var expiring []string
	for _, minted := range resolved.minted {
		if minted.ExpiresAt != nil && containsString(resolved.profile.capabilities, minted.Capability) {
			expiring = append(expiring, minted.Capability)
		}
	}
	return expiring
}

// Refresh implements httpapi.CredentialRefreshService.
func (s *daemonCredentialService) Refresh(ctx context.Context, token string, request httpapi.CredentialRefreshRequest) (httpapi.CredentialResolveResponse, error) {
	grant, err := s.admitGrant(token, request.Capability)
	if err != nil {
		return httpapi.CredentialResolveResponse{}, err
	}
	resolveRequest := httpapi.CredentialResolveRequest{
		RunID: grant.RunID, Stage: grant.Stage, Capabilities: []string{request.Capability},
	}
	resolved, err := s.resolveStage(ctx, resolveRequest, stageResolveMode{
		marker: credentialRefreshMarker, deterministicOnly: true, attempt: grant.Attempt,
	})
	if err != nil {
		return httpapi.CredentialResolveResponse{}, err
	}
	return resolved.response(resolveRequest), nil
}

// admitGrant verifies the grant, confines the capability to it, and charges
// the grant's rate limit.
func (s *daemonCredentialService) admitGrant(token, capabilityName string) (podauth.CredentialGrant, error) {
	if s.grants == nil {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusServiceUnavailable,
			"credentials_unavailable", "mid-stage credential refresh is not enabled on this daemon")
	}
	grant, err := s.grants.key.VerifyCredentialGrant(token)
	if errors.Is(err, podauth.ErrExpiredCredentialGrant) {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusUnauthorized, "credential_grant_expired",
			"the credential-refresh grant has expired")
	}
	if err != nil {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusUnauthorized, "credential_grant_invalid",
			"the credential-refresh grant is not valid for this daemon")
	}
	if !grant.Allows(capabilityName) {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusForbidden, "capability_not_granted",
			fmt.Sprintf("capability %q is not in this stage's credential-refresh grant", capabilityName))
	}
	if s.grants.isRevoked(grant) {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusUnauthorized, "credential_grant_revoked",
			"the stage attempt this credential-refresh grant served has finished")
	}
	if !s.grants.allow(token, grant.ExpiresAt) {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusTooManyRequests, "credential_refresh_rate_limited",
			"this credential-refresh grant is refreshing too often; retry later")
	}
	return grant, nil
}

func (g *stageGrantIssuer) revoke(id string, expiresAt time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(g.now())
	g.revoked[id] = expiresAt
}

func (g *stageGrantIssuer) isRevoked(grant podauth.CredentialGrant) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, revoked := g.revoked[grant.ID]
	return revoked
}

// allow charges one refresh against the grant's token bucket.
func (g *stageGrantIssuer) allow(token string, expiresAt time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.pruneLocked(now)
	digest := sha256.Sum256([]byte(token))
	bucket, ok := g.buckets[digest]
	if !ok {
		bucket = &grantBucket{tokens: grantRefreshBurst, last: now, expiresAt: expiresAt}
		g.buckets[digest] = bucket
	}
	bucket.tokens = min(float64(grantRefreshBurst), bucket.tokens+now.Sub(bucket.last).Seconds()/grantRefreshInterval.Seconds())
	bucket.last = now
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// pruneLocked drops state for grants that have expired anyway, so the maps
// stay bounded by live stages rather than by history.
func (g *stageGrantIssuer) pruneLocked(now time.Time) {
	for key, expiresAt := range g.revoked {
		if !expiresAt.After(now) {
			delete(g.revoked, key)
		}
	}
	for digest, bucket := range g.buckets {
		if !bucket.expiresAt.After(now) {
			delete(g.buckets, digest)
		}
	}
}

// stageGrantMinters maps an absolute instance root to the minter its local
// runners deliver grants from. The daemon registers itself once its API is
// bound; a process that never registers (goobers run, a worker) mints none.
var stageGrantMinters sync.Map

func registerStageGrantMinter(root string, minter executor.StageCredentialGrants) {
	if absolute, err := filepath.Abs(root); err == nil {
		stageGrantMinters.Store(absolute, minter)
	}
}

// stageGrantMinterFor returns the minter registered for instanceRoot, or nil.
func stageGrantMinterFor(instanceRoot string) executor.StageCredentialGrants {
	absolute, err := filepath.Abs(instanceRoot)
	if err != nil {
		return nil
	}
	minter, ok := stageGrantMinters.Load(absolute)
	if !ok {
		return nil
	}
	return minter.(executor.StageCredentialGrants)
}
