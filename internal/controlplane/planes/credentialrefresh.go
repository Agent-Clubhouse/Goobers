package planes

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

const (
	grantRefreshBurst    = 6
	grantRefreshInterval = 20 * time.Second
)

type StageGrantIssuer struct {
	key      *podauth.SignedKey
	endpoint string
	now      func() time.Time
	mu       sync.Mutex
	revoked  map[string]time.Time
	buckets  map[[sha256.Size]byte]*grantBucket
}

type grantBucket struct {
	tokens    float64
	last      time.Time
	expiresAt time.Time
}

func NewStageGrantIssuer(key *podauth.SignedKey, endpoint string) (*StageGrantIssuer, error) {
	if key == nil {
		return nil, errors.New("credential grant signing key is required")
	}
	return &StageGrantIssuer{key: key, endpoint: endpoint, now: time.Now, revoked: map[string]time.Time{}, buckets: map[[sha256.Size]byte]*grantBucket{}}, nil
}

func LocalCredentialEndpoint(listenAddress string, tls bool) (string, error) {
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

func (g *StageGrantIssuer) Key() *podauth.SignedKey { return g.key }
func (g *StageGrantIssuer) Endpoint() string        { return g.endpoint }

func (g *StageGrantIssuer) MintStageGrant(runID, stage string, attempt int32, capabilities []string, ttl time.Duration, shared *journal.RegistryScrubber) (executor.StageCredentialGrant, error) {
	token, grant, err := g.key.MintCredentialGrant(podauth.CredentialGrant{RunID: runID, Stage: stage, Attempt: attempt, Capabilities: capabilities}, ttl)
	if err != nil {
		return executor.StageCredentialGrant{}, err
	}
	shared.RegisterUntil([]byte(token), grant.ExpiresAt)
	var once sync.Once
	return executor.StageCredentialGrant{Endpoint: g.endpoint, Token: token, Revoke: func() { once.Do(func() { g.revoke(grant.ID, grant.ExpiresAt) }) }}, nil
}

func (g *StageGrantIssuer) MintPodGrant(request httpapi.CredentialResolveRequest, capabilities []string, ttl time.Duration, shared *journal.RegistryScrubber) *httpapi.CredentialGrantDelivery {
	token, grant, err := g.key.MintCredentialGrant(podauth.CredentialGrant{
		RunID: request.RunID, Stage: request.Stage, Attempt: request.Attempt, Capabilities: capabilities,
	}, ttl)
	if err != nil {
		return nil
	}
	shared.RegisterUntil([]byte(token), grant.ExpiresAt)
	return &httpapi.CredentialGrantDelivery{Token: token, ExpiresAt: grant.ExpiresAt}
}

func (g *StageGrantIssuer) Admit(token, capabilityName string) (podauth.CredentialGrant, error) {
	if g == nil {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusServiceUnavailable, "credentials_unavailable", "mid-stage credential refresh is not enabled on this daemon")
	}
	grant, err := g.key.VerifyCredentialGrant(token)
	if errors.Is(err, podauth.ErrExpiredCredentialGrant) {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusUnauthorized, "credential_grant_expired", "the credential-refresh grant has expired")
	}
	if err != nil {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusUnauthorized, "credential_grant_invalid", "the credential-refresh grant is not valid for this daemon")
	}
	if !grant.Allows(capabilityName) {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusForbidden, "capability_not_granted", fmt.Sprintf("capability %q is not in this stage's credential-refresh grant", capabilityName))
	}
	if g.isRevoked(grant) {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusUnauthorized, "credential_grant_revoked", "the stage attempt this credential-refresh grant served has finished")
	}
	if !g.allow(token, grant.ExpiresAt) {
		return podauth.CredentialGrant{}, credentialPlaneError(http.StatusTooManyRequests, "credential_refresh_rate_limited", "this credential-refresh grant is refreshing too often; retry later")
	}
	return grant, nil
}

func credentialPlaneError(status int, code, message string) error {
	return httpapi.NewInterventionError(status, code, message, nil)
}

func (g *StageGrantIssuer) revoke(id string, expiresAt time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(g.now())
	g.revoked[id] = expiresAt
}

func (g *StageGrantIssuer) isRevoked(grant podauth.CredentialGrant) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, revoked := g.revoked[grant.ID]
	return revoked
}

func (g *StageGrantIssuer) allow(token string, expiresAt time.Time) bool {
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

func (g *StageGrantIssuer) pruneLocked(now time.Time) {
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
