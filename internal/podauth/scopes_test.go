package podauth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

// scopes_test.go covers the token half of Goobers#3897: a bearer that names
// the planes it may reach, so the dispatcher can hand a stage subprocess
// authority over the claims plane WITHOUT also handing it the authority to
// surrender that stage's own result.

// The restatement in internal/httpapi must be this package's originals.
// httpapi cannot check this itself — podauth imports httpapi for the Principal
// type, so the comparison is only possible from this side.
func TestPodPlaneScopesMatchPodauth(t *testing.T) {
	for _, pair := range []struct{ mine, theirs, who string }{
		{ScopeClaims, httpapi.ScopeClaims, "claims"},
		{ScopeState, httpapi.ScopeState, "state"},
		{ScopeJournal, httpapi.ScopeJournal, "journal"},
		{ScopeTelemetry, httpapi.ScopeTelemetry, "telemetry"},
		{ScopeSurrender, httpapi.ScopeSurrender, "surrender"},
		{ScopeBlob, httpapi.ScopeBlob, "blob"},
		{ScopeCredential, httpapi.ScopeCredential, "credential"},
	} {
		if pair.mine != pair.theirs {
			t.Errorf("%s scope: podauth signs %q, httpapi authorizes on %q", pair.who, pair.mine, pair.theirs)
		}
	}
	// And nothing mintable is missing from the restatement.
	restated := []string{
		httpapi.ScopeClaims, httpapi.ScopeState, httpapi.ScopeJournal, httpapi.ScopeTelemetry,
		httpapi.ScopeSurrender, httpapi.ScopeBlob, httpapi.ScopeCredential,
	}
	for _, scope := range KnownScopes {
		if !slices.Contains(restated, scope) {
			t.Errorf("scope %q is mintable but httpapi restates no constant for it, so no route can require it", scope)
		}
	}
	if len(KnownScopes) != len(restated) {
		t.Errorf("KnownScopes = %v, httpapi restates %v", KnownScopes, restated)
	}
}

// A scoped bearer authenticates to the same run as an unscoped one and
// carries its scopes onto the principal — which is where RequireRoles reads
// them. Asserted through the real Authenticator rather than verify() so the
// path the server actually takes is the one under test.
func TestScopedTokenAuthenticatesWithItsScopes(t *testing.T) {
	for _, minter := range scopedMinters(t) {
		t.Run(minter.name, func(t *testing.T) {
			token, err := minter.MintScoped("run-1", time.Hour, ScopeClaims)
			if err != nil {
				t.Fatalf("MintScoped: %v", err)
			}
			principal := authenticate(t, minter.authenticator, token)
			if principal.Subject != "run:run-1" {
				t.Errorf("subject = %q, want run:run-1", principal.Subject)
			}
			if principal.Issuer != httpapi.PodPrincipalIssuer {
				t.Errorf("issuer = %q, want %q", principal.Issuer, httpapi.PodPrincipalIssuer)
			}
			if !slices.Equal(principal.Scopes, []string{ScopeClaims}) {
				t.Errorf("scopes = %v, want [claims]", principal.Scopes)
			}
			if !principal.HasScope(ScopeClaims) || principal.HasScope(ScopeSurrender) {
				t.Errorf("scopes %v do not confine the principal", principal.Scopes)
			}
		})
	}
}

// The unscoped token stays unscoped — an empty Scopes slice, which HasScope
// reads as "every plane". This is the compatibility path: __dispatch-exec's
// own GOOBERS_POD_TOKEN, and any token an un-upgraded dispatcher minted.
func TestUnscopedTokenCarriesNoScopes(t *testing.T) {
	for _, minter := range scopedMinters(t) {
		t.Run(minter.name, func(t *testing.T) {
			token, err := minter.Mint("run-1", time.Hour)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			principal := authenticate(t, minter.authenticator, token)
			if len(principal.Scopes) != 0 {
				t.Fatalf("scopes = %v, want none (the unscoped pod token)", principal.Scopes)
			}
			for _, scope := range KnownScopes {
				if !principal.HasScope(scope) {
					t.Errorf("the unscoped pod token was refused the %s plane", scope)
				}
			}
		})
	}
}

// A mint naming a scope this build does not know is refused at MINT time.
// The alternative — signing it and letting the verifier sort it out — puts a
// token into the world whose authority no process can describe.
func TestMintScopedRefusesUnknownScopes(t *testing.T) {
	for _, minter := range scopedMinters(t) {
		t.Run(minter.name, func(t *testing.T) {
			for _, bad := range []string{"admin", "", "claims,surrender", "CLAIMS", "clai ms"} {
				if _, err := minter.MintScoped("run-1", time.Hour, bad); !errors.Is(err, ErrUnknownScope) {
					t.Errorf("MintScoped(%q) err = %v, want ErrUnknownScope", bad, err)
				}
			}
		})
	}
}

// Duplicate scopes normalize rather than accumulating, and order does not
// change the token's authority — the payload is a set, written in a stable
// order, so two mints of the same authority are the same authority.
func TestMintScopedNormalizesScopeOrderAndDuplicates(t *testing.T) {
	key := testSignedKey(t)
	a, err := key.MintScoped("run-1", time.Hour, ScopeJournal, ScopeClaims, ScopeClaims)
	if err != nil {
		t.Fatalf("MintScoped: %v", err)
	}
	b, err := key.MintScoped("run-1", time.Hour, ScopeClaims, ScopeJournal)
	if err != nil {
		t.Fatalf("MintScoped: %v", err)
	}
	runA, scopesA, errA := key.verifySigned(a)
	runB, scopesB, errB := key.verifySigned(b)
	if errA != nil || errB != nil {
		t.Fatalf("verify: %v / %v", errA, errB)
	}
	if runA != runB || !slices.Equal(scopesA, scopesB) {
		t.Fatalf("same authority verified differently: (%q,%v) vs (%q,%v)", runA, scopesA, runB, scopesB)
	}
	if !slices.Equal(scopesA, []string{ScopeClaims, ScopeJournal}) {
		t.Fatalf("scopes = %v, want a deduplicated, stably ordered [claims journal]", scopesA)
	}
}

// BACKWARD COMPATIBILITY, both directions, stated as bytes.
//
// An unscoped token's payload must be EXACTLY the two-segment form this code
// emitted before scopes existed, because a daemon that has not been
// redeployed still has to verify tokens a rolled-forward worker mints. And a
// SCOPED token must be one the old verifier refuses — which the three-segment
// form guarantees, since the old parser required exactly two.
func TestSignedPayloadIsBackwardCompatible(t *testing.T) {
	unscoped := signedPayload("run-1", nil, 1700000000)
	want := base64.RawURLEncoding.EncodeToString([]byte("run-1")) + "." + strconv.FormatInt(1700000000, 10)
	if unscoped != want {
		t.Fatalf("unscoped payload = %q, want the pre-scopes form %q; an un-redeployed daemon would reject every token", unscoped, want)
	}
	if got := strings.Count(unscoped, "."); got != 1 {
		t.Fatalf("unscoped payload has %d delimiters, want the legacy 1", got)
	}
	scoped := signedPayload("run-1", []string{ScopeClaims}, 1700000000)
	if got := strings.Count(scoped, "."); got != 2 {
		t.Fatalf("scoped payload = %q has %d delimiters, want 3 segments so an old verifier refuses it", scoped, got)
	}
	// The old verifier's behaviour, reproduced: exactly-two-segments.
	if len(strings.Split(scoped, ".")) == 2 {
		t.Fatal("a scoped token parses as the legacy shape; an old daemon would admit it with no scope enforcement at all")
	}
}

// A payload carrying an explicitly EMPTY scope segment must not verify as the
// unscoped (all-planes) token. That is the one widening a truncation could
// produce, so it is refused rather than normalized.
func TestEmptyScopeSegmentIsRefused(t *testing.T) {
	key := testSignedKey(t)
	exp := time.Now().Add(time.Hour).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte("run-1")) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("")) + "." + strconv.FormatInt(exp, 10)
	if _, _, err := key.verifySigned(tokenPrefix + payload + "." + key.sign(payload)); err == nil {
		t.Fatal("an explicitly empty scope segment verified; it must not widen to the unscoped token")
	}
}

// A scope segment naming an unknown plane is refused at VERIFY too, not
// narrowed to the known ones — the fail-closed direction for a peer that is
// ahead of this build.
func TestUnknownScopeInAValidSignatureIsRefused(t *testing.T) {
	key := testSignedKey(t)
	exp := time.Now().Add(time.Hour).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte("run-1")) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("claims,root")) + "." + strconv.FormatInt(exp, 10)
	token := tokenPrefix + payload + "." + key.sign(payload)
	if _, _, err := key.verifySigned(token); !errors.Is(err, ErrMalformedToken) {
		t.Fatalf("err = %v, want ErrMalformedToken for a correctly signed token naming an unknown plane", err)
	}
}

// Scopes are inside the signature: widening one is a forgery, not an edit.
func TestScopesCannotBeWidenedWithoutTheKey(t *testing.T) {
	key := testSignedKey(t)
	token, err := key.MintScoped("run-1", time.Hour, ScopeClaims)
	if err != nil {
		t.Fatalf("MintScoped: %v", err)
	}
	body := strings.TrimPrefix(token, tokenPrefix)
	segments := strings.Split(body, ".")
	if len(segments) != 4 {
		t.Fatalf("token body = %q, want run.scopes.exp.sig", body)
	}
	segments[1] = base64.RawURLEncoding.EncodeToString([]byte(ScopeClaims + "," + ScopeSurrender))
	forged := tokenPrefix + strings.Join(segments, ".")
	if _, _, err := key.verifySigned(forged); err == nil {
		t.Fatal("a token with its scope list rewritten verified; scopes are not inside the signature")
	}
}

// The run id is still the containment boundary a scope sits inside: a claims
// bearer for run-1 is not a claims bearer for run-2.
func TestScopedTokenIsStillRunBound(t *testing.T) {
	for _, minter := range scopedMinters(t) {
		t.Run(minter.name, func(t *testing.T) {
			token, err := minter.MintScoped("run-1", time.Hour, ScopeClaims)
			if err != nil {
				t.Fatalf("MintScoped: %v", err)
			}
			principal := authenticate(t, minter.authenticator, token)
			if principal.Subject == "run:run-2" {
				t.Fatal("a run-1 bearer authenticated as run-2")
			}
			if principal.Subject != "run:run-1" {
				t.Fatalf("subject = %q, want run:run-1", principal.Subject)
			}
		})
	}
}

// Expiry still bounds a scoped token — the scope segment must not have
// displaced the exp segment's meaning.
func TestScopedTokenStillExpires(t *testing.T) {
	minted := time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC)
	key := testSignedKey(t).WithClock(func() time.Time { return minted })
	token, err := key.MintScoped("run-1", time.Minute, ScopeClaims)
	if err != nil {
		t.Fatalf("MintScoped: %v", err)
	}
	later := testSignedKey(t).WithClock(func() time.Time { return minted.Add(2 * time.Minute) })
	if _, _, err := later.verifySigned(token); err == nil {
		t.Fatal("an expired scoped token verified")
	}
	// ...and still verifies inside its window, so the failure above is the
	// clock and not a payload the scope segment broke.
	if _, _, err := key.verifySigned(token); err != nil {
		t.Fatalf("a live scoped token failed to verify: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

type namedMinter struct {
	name          string
	authenticator *Authenticator
	Mint          func(string, time.Duration) (string, error)
	MintScoped    func(string, time.Duration, ...string) (string, error)
}

// Both production minters, exercised by every scope test: the in-memory
// Registry (single-process) and the HMAC SignedKey (mode-3, where the worker
// that mints and the daemon that verifies are different processes).
func scopedMinters(t *testing.T) []namedMinter {
	t.Helper()
	registry := NewRegistry()
	key := testSignedKey(t)
	return []namedMinter{
		{"registry", newTestAuthenticator(t, registry), registry.Mint, registry.MintScoped},
		{"signed key", newTestAuthenticator(t, key), key.Mint, key.MintScoped},
	}
}

func newTestAuthenticator(t *testing.T, verifier Verifier) *Authenticator {
	t.Helper()
	authenticator, err := NewAuthenticator(verifier, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return authenticator
}

func testSignedKey(t *testing.T) *SignedKey {
	t.Helper()
	key, err := NewSignedKey([]byte(strings.Repeat("k", MinSignedKeyBytes)))
	if err != nil {
		t.Fatalf("NewSignedKey: %v", err)
	}
	return key
}

func authenticate(t *testing.T, authenticator *Authenticator, token string) httpapi.Principal {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/claims/acquire", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	principal, err := authenticator.Authenticate(request)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal == nil {
		t.Fatal("Authenticate returned no principal for a valid token")
	}
	return *principal
}
