package podauth

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

func TestWorkerCredentialIsSeparateFromPodAndHumanIdentities(t *testing.T) {
	now := time.Now()
	signer, err := NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	signer.WithClock(func() time.Time { return now })
	fallback := &stubAuthenticator{principal: &httpapi.Principal{Subject: "human", Roles: []httpapi.Role{httpapi.RoleAdmin}}}
	auth, err := NewAuthenticator(signer, fallback)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.MintWorkerConfigDigest("dispatcher-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.Authenticate(requestWithBearer(token))
	if err != nil || principal == nil || principal.Subject != "worker:dispatcher-a" || principal.Issuer != httpapi.WorkerPrincipalIssuer || len(principal.Roles) != 0 || httpapi.IsPodPrincipal(*principal) {
		t.Fatalf("wrong worker identity: %+v, %v", principal, err)
	}
	pod, err := signer.Mint("real-run", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, forged := range []string{
		tokenPrefix + strings.TrimPrefix(token, workerTokenPrefix),
		workerTokenPrefix + strings.TrimPrefix(pod, tokenPrefix),
		token + "x", workerTokenPrefix + "malformed",
	} {
		if _, err := auth.Authenticate(requestWithBearer(forged)); err == nil {
			t.Fatal("forged identity accepted")
		}
	}
	now = now.Add(time.Minute)
	if _, err := auth.Authenticate(requestWithBearer(token)); err == nil {
		t.Fatal("expired worker accepted")
	}
	if fallback.called {
		t.Fatal("worker credential delegated to human authentication")
	}
	registryAuth, err := NewAuthenticator(NewRegistry(), fallback)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registryAuth.Authenticate(requestWithBearer(token)); err == nil || fallback.called {
		t.Fatal("registry accepted unsupported worker identity")
	}
}

func TestWorkerWireTokenCannotBecomeAScopedPodPayload(t *testing.T) {
	signer, err := NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range KnownScopes {
		token, err := signer.MintWorkerConfigDigest(scope, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		// The worker wire prefix is valid base64. With a MAC over that wire
		// prefix, adding a pod prefix would reinterpret workerID as pod scopes.
		// The non-wire domain must prevent that cross-protocol substitution.
		if _, err := auth.Authenticate(requestWithBearer(tokenPrefix + token)); err == nil {
			t.Fatalf("worker %q credential became a scoped pod token", scope)
		}
	}
}

func TestWorkerCredentialRequiresSameKeyAndBoundedIdentityAndLifetime(t *testing.T) {
	signer, err := NewSignedKey([]byte(strings.Repeat("a", 32)))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewSignedKey([]byte(strings.Repeat("b", 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.MintWorkerConfigDigest("worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.verifyWorkerConfigDigest(token); err == nil {
		t.Fatal("wrong shared key accepted")
	}
	for _, id := range []string{"", " worker", "worker\nline", strings.Repeat("w", 257), string([]byte{255})} {
		if _, err := signer.MintWorkerConfigDigest(id, time.Minute); err == nil {
			t.Fatal("invalid identity minted")
		}
	}
	for _, ttl := range []time.Duration{0, -time.Second, MaxWorkerTokenTTL + time.Second} {
		if _, err := signer.MintWorkerConfigDigest("worker", ttl); err == nil {
			t.Fatal("invalid lifetime minted")
		}
	}
}
