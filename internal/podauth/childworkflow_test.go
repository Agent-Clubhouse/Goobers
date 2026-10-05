package podauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

func childGrantFixture() ChildWorkflowGrant {
	return ChildWorkflowGrant{Gaggle: "web", RunID: "parent-1", StageOccurrence: "parallel/review/visit-2", AttemptID: "attempt-3",
		ConfigDigest: "sha256:" + strings.Repeat("a", 64), PolicyDigest: "sha256:" + strings.Repeat("b", 64)}
}

func TestChildGrantBindsOccurrenceAndAttempt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	token, minted, err := key.MintChildWorkflowGrant(childGrantFixture(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := key.VerifyChildWorkflowGrant(token)
	if err != nil || !reflect.DeepEqual(got, minted) || got.ID == "" {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	_, second, err := key.MintChildWorkflowGrant(childGrantFixture(), time.Hour)
	if err != nil || second.ID == got.ID {
		t.Fatal("replacement grants must have independent revocation identities")
	}
	auth, err := NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	auth.WithChildWorkflowGrants(key)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/parent-1/child-workflows/start", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	principal, err := auth.Authenticate(r)
	if err != nil || principal == nil || principal.Issuer != httpapi.ChildWorkflowPrincipalIssuer || principal.ChildWorkflow == nil {
		t.Fatalf("authenticate = %+v %v", principal, err)
	}
	if len(principal.Roles) != 0 || len(principal.Scopes) != 0 || httpapi.IsPodPrincipal(*principal) {
		t.Fatal("child grant was promoted to general human or pod authority")
	}
	claim := principal.ChildWorkflow
	if claim.GrantID != minted.ID || claim.RunID != minted.RunID || claim.Gaggle != minted.Gaggle || claim.StageOccurrence != minted.StageOccurrence || claim.AttemptID != minted.AttemptID || claim.ConfigDigest != minted.ConfigDigest || claim.PolicyDigest != minted.PolicyDigest {
		t.Fatal("authenticated origin lost a signed authority field")
	}
}

func TestChildGrantCannotBecomeAnotherCredential(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	token, _, err := key.MintChildWorkflowGrant(childGrantFixture(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grantKey(t, 8, &now).VerifyChildWorkflowGrant(token); !errors.Is(err, ErrInvalidChildWorkflowGrant) {
		t.Fatalf("foreign key accepted: %v", err)
	}
	rest := strings.TrimPrefix(token, ChildWorkflowGrantPrefix)
	if _, err := key.VerifyCredentialGrant(CredentialGrantPrefix + rest); err == nil {
		t.Fatal("child MAC accepted as credential refresh grant")
	}
	if _, _, err := key.verifySigned(tokenPrefix + rest); err == nil {
		t.Fatal("child MAC accepted as pod credential")
	}
	credential, _, err := key.MintCredentialGrant(testCredentialGrant(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := key.VerifyChildWorkflowGrant(ChildWorkflowGrantPrefix + strings.TrimPrefix(credential, CredentialGrantPrefix)); err == nil {
		t.Fatal("credential-refresh MAC accepted as child authority")
	}
	payload, mac, _ := strings.Cut(rest, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	raw = []byte(strings.Replace(string(raw), "parent-1", "sibling-2", 1))
	forged := ChildWorkflowGrantPrefix + base64.RawURLEncoding.EncodeToString(raw) + "." + mac
	if _, err := key.VerifyChildWorkflowGrant(forged); err == nil {
		t.Fatal("modified parent accepted")
	}
	now = now.Add(time.Minute)
	if _, err := key.VerifyChildWorkflowGrant(token); !errors.Is(err, ErrExpiredChildWorkflowGrant) {
		t.Fatalf("expired grant: %v", err)
	}
}

func TestChildGrantRefusesInvalidAndOversizedClaims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	for _, change := range []func(*ChildWorkflowGrant){
		func(g *ChildWorkflowGrant) { g.Gaggle = "" },
		func(g *ChildWorkflowGrant) { g.RunID = " wrong " },
		func(g *ChildWorkflowGrant) { g.RunID = "../sibling" },
		func(g *ChildWorkflowGrant) { g.RunID = "parent/other" },
		func(g *ChildWorkflowGrant) { g.RunID = "parent%2fsibling" },
		func(g *ChildWorkflowGrant) { g.StageOccurrence = "branch\x00visit" },
		func(g *ChildWorkflowGrant) { g.AttemptID = strings.Repeat("a", 129) },
		func(g *ChildWorkflowGrant) { g.ConfigDigest = strings.Repeat("a", 64) },
		func(g *ChildWorkflowGrant) { g.PolicyDigest = "sha256:" + strings.Repeat("Z", 64) },
	} {
		claim := childGrantFixture()
		change(&claim)
		if _, _, err := key.MintChildWorkflowGrant(claim, time.Hour); err == nil {
			t.Fatal("invalid claim minted")
		}
	}
	for _, ttl := range []time.Duration{0, -time.Second, MaxChildWorkflowGrantTTL + time.Second} {
		if _, _, err := key.MintChildWorkflowGrant(childGrantFixture(), ttl); err == nil {
			t.Fatalf("invalid TTL accepted: %s", ttl)
		}
	}
	if _, err := key.VerifyChildWorkflowGrant(ChildWorkflowGrantPrefix + strings.Repeat("a", maxChildWorkflowGrantBytes)); err == nil {
		t.Fatal("oversized token accepted")
	}
	claim := childGrantFixture()
	claim.ID = "nonce"
	raw, _ := json.Marshal(childWorkflowPayload{ChildWorkflowGrant: claim, Exp: now.Add(time.Hour).Unix()})
	raw = append(raw[:len(raw)-1], []byte(`,"admin":true}`)...)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := key.VerifyChildWorkflowGrant(ChildWorkflowGrantPrefix + payload + "." + key.sign(childWorkflowMACDomain+payload)); err == nil {
		t.Fatal("unknown signed claim accepted")
	}
}

func TestChildGrantReservedPrefixNeverFallsBack(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := grantKey(t, 7, &now)
	token, _, err := key.MintChildWorkflowGrant(childGrantFixture(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fallback := &stubAuthenticator{principal: &httpapi.Principal{Subject: "admin", Roles: []httpapi.Role{httpapi.RoleAdmin}}}
	auth, err := NewAuthenticator(NewRegistry(), fallback)
	if err != nil {
		t.Fatal(err)
	}
	if principal, err := auth.Authenticate(requestWithBearer(token)); err == nil || principal != nil || fallback.called {
		t.Fatal("disabled child grants reached fallback or authenticated")
	}
	auth.WithChildWorkflowGrants(key)
	if principal, err := auth.Authenticate(requestWithBearer(token)); err != nil || principal.ChildWorkflow == nil {
		t.Fatalf("local pod registry with separate child key: %v", err)
	}
	for _, invalid := range []string{ChildWorkflowGrantPrefix, ChildWorkflowGrantPrefix + "malformed"} {
		if principal, err := auth.Authenticate(requestWithBearer(invalid)); err == nil || principal != nil || fallback.called {
			t.Fatal("invalid child grant reached fallback or authenticated")
		}
	}
	now = now.Add(time.Minute)
	if principal, err := auth.Authenticate(requestWithBearer(token)); !errors.Is(err, ErrExpiredChildWorkflowGrant) || principal != nil || fallback.called {
		t.Fatal("expired child grant reached fallback or authenticated")
	}
}

func TestChildGrantRecoveryRetainsExactExpiryAndNonce(t *testing.T) {
	now := time.Unix(1700000000, 0)
	key := grantKey(t, 7, &now)
	token, grant, err := key.MintChildWorkflowGrant(childGrantFixture(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	recovered, err := key.RecoverChildWorkflowGrant(grant)
	if err != nil || recovered != token {
		t.Fatalf("recovery changed bearer: %v", err)
	}
	now = grant.ExpiresAt
	if _, err = key.RecoverChildWorkflowGrant(grant); err == nil {
		t.Fatal("expired grant recovered")
	}
}
