package planes

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

func newTestStageGrantIssuer(t *testing.T, endpoint string) *StageGrantIssuer {
	t.Helper()
	key, err := podauth.CredentialGrantSigningKey("")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewStageGrantIssuer(key, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return issuer
}

func TestLocalCredentialEndpoint(t *testing.T) {
	for _, test := range []struct {
		name    string
		address string
		tls     bool
		want    string
	}{
		{name: "loopback", address: "127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{name: "unspecified", address: "0.0.0.0:8080", want: "http://127.0.0.1:8080"},
		{name: "tls", address: "[::]:8080", tls: true, want: "https://127.0.0.1:8080"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := LocalCredentialEndpoint(test.address, test.tls)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("endpoint = %q, want %q", got, test.want)
			}
		})
	}
	if _, err := LocalCredentialEndpoint("missing-port", false); err == nil {
		t.Fatal("invalid listen address succeeded")
	}
}

func TestStageGrantIssuerAdmitsRevokesAndRateLimits(t *testing.T) {
	issuer := newTestStageGrantIssuer(t, "http://127.0.0.1:8080")
	registry := journal.NewRegistryScrubber()
	delivery, err := issuer.MintStageGrant("run-1", "build", 2, []string{"repo:push"}, time.Hour, registry)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := issuer.Admit(delivery.Token, "repo:push")
	if err != nil {
		t.Fatal(err)
	}
	if grant.RunID != "run-1" || grant.Stage != "build" || grant.Attempt != 2 {
		t.Fatalf("grant = %+v", grant)
	}
	if !strings.Contains(string(registry.Scrub([]byte(delivery.Token))), journal.Redacted) {
		t.Fatal("minted grant was not registered with the scrubber")
	}
	if _, err := issuer.Admit(delivery.Token, "issues:write"); !hasIntervention(err, http.StatusForbidden, "capability_not_granted") {
		t.Fatalf("outside capability error = %v", err)
	}

	for range grantRefreshBurst - 1 {
		if _, err := issuer.Admit(delivery.Token, "repo:push"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := issuer.Admit(delivery.Token, "repo:push"); !hasIntervention(err, http.StatusTooManyRequests, "credential_refresh_rate_limited") {
		t.Fatalf("burst+1 error = %v", err)
	}

	revoked, err := issuer.MintStageGrant("run-1", "build", 3, []string{"repo:push"}, time.Hour, registry)
	if err != nil {
		t.Fatal(err)
	}
	revoked.Revoke()
	revoked.Revoke()
	if _, err := issuer.Admit(revoked.Token, "repo:push"); !hasIntervention(err, http.StatusUnauthorized, "credential_grant_revoked") {
		t.Fatalf("revoked grant error = %v", err)
	}
}

func TestStageGrantIssuerRejectsUnavailableAndForeignGrants(t *testing.T) {
	if _, err := (*StageGrantIssuer)(nil).Admit("token", "repo:push"); !hasIntervention(err, http.StatusServiceUnavailable, "credentials_unavailable") {
		t.Fatalf("nil issuer error = %v", err)
	}
	first := newTestStageGrantIssuer(t, "http://127.0.0.1:8080")
	second := newTestStageGrantIssuer(t, "http://127.0.0.1:8081")
	token, _, err := first.Key().MintCredentialGrant(podauth.CredentialGrant{
		RunID: "run-1", Stage: "build", Capabilities: []string{"repo:push"},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Admit(token, "repo:push"); !hasIntervention(err, http.StatusUnauthorized, "credential_grant_invalid") {
		t.Fatalf("foreign grant error = %v", err)
	}
}

func TestStageGrantIssuerMintsPodGrantAndPrunesExpiredState(t *testing.T) {
	issuer := newTestStageGrantIssuer(t, "http://127.0.0.1:8080")
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	issuer.now = func() time.Time { return now }
	issuer.Key().WithClock(func() time.Time { return now })
	registry := journal.NewRegistryScrubber()
	request := httpapi.CredentialResolveRequest{RunID: "run-1", Stage: "build", Attempt: 4}
	delivery := issuer.MintPodGrant(request, []string{"repo:push"}, time.Minute, registry)
	if delivery == nil {
		t.Fatal("pod grant was not minted")
	}
	grant, err := issuer.Key().VerifyCredentialGrant(delivery.Token)
	if err != nil {
		t.Fatal(err)
	}
	if grant.RunID != request.RunID || grant.Stage != request.Stage || grant.Attempt != request.Attempt || !grant.Allows("repo:push") {
		t.Fatalf("pod grant = %+v", grant)
	}
	if !strings.Contains(string(registry.Scrub([]byte(delivery.Token))), journal.Redacted) {
		t.Fatal("pod grant was not registered with the scrubber")
	}
	if _, err := issuer.Admit(delivery.Token, "repo:push"); err != nil {
		t.Fatal(err)
	}
	issuer.revoke(grant.ID, grant.ExpiresAt)
	digest := sha256.Sum256([]byte(delivery.Token))
	if _, ok := issuer.buckets[digest]; !ok {
		t.Fatal("admitted token has no rate-limit bucket")
	}
	now = now.Add(2 * time.Minute)
	if _, err := issuer.Admit(delivery.Token, "repo:push"); !hasIntervention(err, http.StatusUnauthorized, "credential_grant_expired") {
		t.Fatalf("expired grant error = %v", err)
	}
	issuer.allow("current-token", now.Add(time.Hour))
	if _, ok := issuer.revoked[grant.ID]; ok {
		t.Fatal("expired revocation was not pruned")
	}
	if _, ok := issuer.buckets[digest]; ok {
		t.Fatal("expired rate-limit bucket was not pruned")
	}
}

func hasIntervention(err error, status int, code string) bool {
	var intervention *httpapi.InterventionError
	return errors.As(err, &intervention) && intervention.Status == status && intervention.Code == code
}
