package podauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

// TestVerifyCredentialGrantRefusesMalformedTokens: a token that is not
// shaped like a grant, and a correctly signed payload that does not decode to
// a complete, valid grant, are both ErrInvalidCredentialGrant. The signed
// cases prove the payload checks, not the signature, refuse them.
func TestVerifyCredentialGrantRefusesMalformedTokens(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	signed := func(payload string) string {
		return CredentialGrantPrefix + payload + "." + key.sign(credentialGrantMACDomain+payload)
	}
	encode := func(p credentialGrantPayload) string {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	valid := func() credentialGrantPayload {
		return credentialGrantPayload{
			CredentialGrant: CredentialGrant{ID: "grant-id", RunID: "run-1", Stage: "push-branch", Capabilities: []string{"repo:push"}},
			Exp:             now.Add(time.Hour).Unix(),
		}
	}
	// The control: a hand-signed valid payload verifies, so each refusal
	// below is the payload's fault.
	if got, err := key.VerifyCredentialGrant(signed(encode(valid()))); err != nil || got.ID != "grant-id" || !got.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("hand-signed valid grant = %+v, %v", got, err)
	}
	mutated := func(mutate func(*credentialGrantPayload)) string {
		p := valid()
		mutate(&p)
		return signed(encode(p))
	}
	manyCaps := make([]string, maxCredentialGrantCapabilities+1)
	for i := range manyCaps {
		manyCaps[i] = "cap-" + strings.Repeat("x", i+1)
	}
	for name, token := range map[string]string{
		"pod token prefix":         "goobers-pod.payload.mac",
		"oversized":                CredentialGrantPrefix + strings.Repeat("a", maxCredentialGrantBytes),
		"no MAC separator":         CredentialGrantPrefix + "payload",
		"empty payload":            CredentialGrantPrefix + "." + key.sign(credentialGrantMACDomain),
		"empty MAC":                CredentialGrantPrefix + "payload.",
		"dotted MAC":               CredentialGrantPrefix + "payload.mac.extra",
		"signed, not base64url":    signed("not*base64"),
		"signed, not JSON":         signed(base64.RawURLEncoding.EncodeToString([]byte("not json"))),
		"signed, no run":           mutated(func(p *credentialGrantPayload) { p.RunID = " " }),
		"signed, negative attempt": mutated(func(p *credentialGrantPayload) { p.Attempt = -1 }),
		"signed, too many caps":    mutated(func(p *credentialGrantPayload) { p.Capabilities = manyCaps }),
		"signed, no expiry":        mutated(func(p *credentialGrantPayload) { p.Exp = 0 }),
		"signed, no grant ID":      mutated(func(p *credentialGrantPayload) { p.ID = "" }),
		"signed, blank capability": mutated(func(p *credentialGrantPayload) { p.Capabilities = []string{"repo:push", ""} }),
	} {
		if got, err := key.VerifyCredentialGrant(token); !errors.Is(err, ErrInvalidCredentialGrant) || got.RunID != "" {
			t.Errorf("%s: verify = %+v, %v; want ErrInvalidCredentialGrant", name, got, err)
		}
	}
}

func TestMintCredentialGrantRefusesUnboundedClaims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	manyCaps := make([]string, maxCredentialGrantCapabilities+1)
	for i := range manyCaps {
		manyCaps[i] = "cap-" + strings.Repeat("x", i+1)
	}
	for name, tc := range map[string]struct {
		grant CredentialGrant
		ttl   time.Duration
		want  string
	}{
		"negative attempt": {CredentialGrant{RunID: "r", Stage: "s", Attempt: -1, Capabilities: []string{"repo:push"}}, time.Hour, "podauth: credential grant attempt must not be negative"},
		"too many caps":    {CredentialGrant{RunID: "r", Stage: "s", Capabilities: manyCaps}, time.Hour, "podauth: credential grant names more than 32 capabilities"},
		"negative TTL":     {CredentialGrant{RunID: "r", Stage: "s", Capabilities: []string{"repo:push"}}, -time.Second, "podauth: credential grant TTL must be positive, got -1s"},
	} {
		token, minted, err := key.MintCredentialGrant(tc.grant, tc.ttl)
		if err == nil || err.Error() != tc.want || token != "" || minted.ID != "" {
			t.Errorf("%s: mint = %q, %+v, %v; want %q", name, token, minted, err, tc.want)
		}
	}
	// The bound is inclusive: exactly the maximum mints.
	if _, _, err := key.MintCredentialGrant(CredentialGrant{RunID: "r", Stage: "s", Capabilities: manyCaps[:maxCredentialGrantCapabilities]}, time.Hour); err != nil {
		t.Errorf("mint at the capability bound: %v", err)
	}
}
