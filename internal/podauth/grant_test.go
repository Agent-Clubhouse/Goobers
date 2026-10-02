package podauth

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

func grantKey(t *testing.T, fill byte, now *time.Time) *SignedKey {
	t.Helper()
	key, err := NewSignedKey(bytes.Repeat([]byte{fill}, MinSignedKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	return key.WithClock(func() time.Time { return *now })
}

func testCredentialGrant() CredentialGrant {
	return CredentialGrant{RunID: "run-1", Stage: "push-branch", Attempt: 2, Capabilities: []string{"repo:push", "github:pr:write", "repo:push"}}
}

func TestCredentialGrantRoundTrips(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	token, minted, err := key.MintCredentialGrant(testCredentialGrant(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !IsCredentialGrant(token) || !strings.HasPrefix(token, httpapi.CredentialGrantTokenPrefix) {
		t.Fatalf("token %q lacks the grant prefix", token)
	}
	got, err := key.VerifyCredentialGrant(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.RunID != "run-1" || got.Stage != "push-branch" || got.Attempt != 2 || !got.ExpiresAt.Equal(minted.ExpiresAt) {
		t.Fatalf("claims = %+v, minted %+v", got, minted)
	}
	if strings.Join(got.Capabilities, ",") != "github:pr:write,repo:push" {
		t.Fatalf("capabilities = %v, want the normalized declared set", got.Capabilities)
	}
	if !got.Allows("repo:push") || got.Allows("github:pr:merge") || got.Allows("") {
		t.Fatal("Allows does not confine the grant to its capability list")
	}
}

// TestCredentialGrantPrefixMatchesHTTPAPI pins httpapi's restated prefix.
func TestCredentialGrantPrefixMatchesHTTPAPI(t *testing.T) {
	if CredentialGrantPrefix != httpapi.CredentialGrantTokenPrefix {
		t.Fatalf("podauth %q != httpapi %q", CredentialGrantPrefix, httpapi.CredentialGrantTokenPrefix)
	}
}

func TestCredentialGrantRefusesExpiredForgedAndForeign(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	other := grantKey(t, 9, &now)
	token, _, err := key.MintCredentialGrant(testCredentialGrant(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Another daemon's key (a restarted daemon has a fresh one).
	if _, err := other.VerifyCredentialGrant(token); !errors.Is(err, ErrInvalidCredentialGrant) {
		t.Fatalf("foreign key verify = %v, want ErrInvalidCredentialGrant", err)
	}
	// A widened payload: swap the claims for another stage's, keep the MAC.
	widened, _, err := key.MintCredentialGrant(CredentialGrant{RunID: "run-2", Stage: "merge", Capabilities: []string{"github:pr:merge"}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Split(strings.TrimPrefix(widened, CredentialGrantPrefix), ".")[0]
	mac := strings.Split(strings.TrimPrefix(token, CredentialGrantPrefix), ".")[1]
	if _, err := key.VerifyCredentialGrant(CredentialGrantPrefix + payload + "." + mac); !errors.Is(err, ErrInvalidCredentialGrant) {
		t.Fatalf("spliced grant verify = %v, want ErrInvalidCredentialGrant", err)
	}
	// A pod token signed by the same key is not a grant, and a grant is not
	// a pod token.
	pod, err := key.Mint("run-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := key.VerifyCredentialGrant(CredentialGrantPrefix + strings.TrimPrefix(pod, tokenPrefix)); err == nil {
		t.Fatal("a pod token re-prefixed as a grant verified")
	}
	if _, _, err := key.verifySigned(tokenPrefix + strings.TrimPrefix(token, CredentialGrantPrefix)); err == nil {
		t.Fatal("a grant re-prefixed as a pod token verified")
	}
	// Expiry.
	now = now.Add(time.Minute)
	if _, err := key.VerifyCredentialGrant(token); !errors.Is(err, ErrExpiredCredentialGrant) {
		t.Fatalf("expired verify = %v, want ErrExpiredCredentialGrant", err)
	}
}

func TestCredentialGrantTTLIsCappedAndClaimsRequired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	_, minted, err := key.MintCredentialGrant(testCredentialGrant(), 1000*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := minted.ExpiresAt.Sub(now); got != MaxCredentialGrantTTL {
		t.Fatalf("ttl = %s, want the %s ceiling", got, MaxCredentialGrantTTL)
	}
	for name, grant := range map[string]CredentialGrant{
		"no run":          {Stage: "s", Capabilities: []string{"repo:push"}},
		"no stage":        {RunID: "r", Capabilities: []string{"repo:push"}},
		"no capabilities": {RunID: "r", Stage: "s"},
		"blank cap":       {RunID: "r", Stage: "s", Capabilities: []string{" "}},
	} {
		if _, _, err := key.MintCredentialGrant(grant, time.Hour); err == nil {
			t.Errorf("%s: mint succeeded", name)
		}
	}
	if _, _, err := key.MintCredentialGrant(testCredentialGrant(), 0); err == nil {
		t.Error("zero TTL minted")
	}
}

// TestAuthenticatorAdmitsGrantsOnlyWhenConfigured: a grant-prefixed bearer is
// a grant principal with a configured key, refused without one, and never
// handed to the human authenticator.
func TestAuthenticatorAdmitsGrantsOnlyWhenConfigured(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	token, _, err := key.MintCredentialGrant(testCredentialGrant(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fallback := &countingAuthenticator{}
	request := httptest.NewRequest("POST", "/api/v1/credentials/refresh", nil)
	request.Header.Set("Authorization", "Bearer "+token)

	without, err := NewAuthenticator(NewRegistry(), fallback)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := without.Authenticate(request); err == nil {
		t.Fatal("a grant authenticated with no grant key configured")
	}
	with, err := NewAuthenticator(NewRegistry(), fallback)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := with.WithCredentialGrants(key).Authenticate(request)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if principal.Issuer != httpapi.CredentialGrantPrincipalIssuer || principal.Subject != "run:run-1" || len(principal.Roles) != 0 || len(principal.Scopes) != 0 {
		t.Fatalf("principal = %+v", principal)
	}
	if fallback.calls != 0 {
		t.Fatalf("a grant bearer reached the human authenticator %d times", fallback.calls)
	}
}

type countingAuthenticator struct{ calls int }

func (c *countingAuthenticator) Authenticate(*http.Request) (*httpapi.Principal, error) {
	c.calls++
	return &httpapi.Principal{Subject: "human", Roles: []httpapi.Role{httpapi.RoleAdmin}}, nil
}
