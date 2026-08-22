package podauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

type stubAuthenticator struct {
	principal *httpapi.Principal
	err       error
	called    bool
}

func (s *stubAuthenticator) Authenticate(*http.Request) (*httpapi.Principal, error) {
	s.called = true
	return s.principal, s.err
}

func requestWithBearer(token string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/claims/acquire", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func TestMintedTokenAuthenticatesAsItsRun(t *testing.T) {
	registry := NewRegistry()
	token, err := registry.Mint("run-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		t.Fatalf("token %q does not carry the pod prefix", token)
	}
	fallback := &stubAuthenticator{}
	authenticator, err := NewAuthenticator(registry, fallback)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authenticator.Authenticate(requestWithBearer(token))
	if err != nil {
		t.Fatal(err)
	}
	if principal.Subject != "run:run-a" || principal.Issuer != httpapi.PodPrincipalIssuer {
		t.Fatalf("principal = %+v", principal)
	}
	if len(principal.Roles) != 0 {
		t.Fatalf("pod principal must hold no instance roles, got %v", principal.Roles)
	}
	if fallback.called {
		t.Fatal("pod token must not reach the fallback authenticator")
	}
}

func TestUnknownExpiredAndRevokedTokensFailClosed(t *testing.T) {
	now := time.Now()
	registry := NewRegistry().WithClock(func() time.Time { return now })
	authenticator, err := NewAuthenticator(registry, &stubAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := authenticator.Authenticate(requestWithBearer(tokenPrefix + "deadbeef")); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("unknown token error = %v, want ErrUnknownToken", err)
	}

	token, err := registry.Mint("run-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := authenticator.Authenticate(requestWithBearer(token)); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("expired token error = %v, want ErrUnknownToken", err)
	}

	now = now.Add(-2 * time.Minute)
	token, err = registry.Mint("run-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	registry.Revoke("run-b")
	if _, err := authenticator.Authenticate(requestWithBearer(token)); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("revoked token error = %v, want ErrUnknownToken", err)
	}
}

func TestNonPodBearersDelegateToFallback(t *testing.T) {
	registry := NewRegistry()
	fallback := &stubAuthenticator{principal: &httpapi.Principal{Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}}
	authenticator, err := NewAuthenticator(registry, fallback)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "eyJhbGciOiJSUzI1NiJ9.x.y"} {
		fallback.called = false
		principal, err := authenticator.Authenticate(requestWithBearer(token))
		if err != nil || principal == nil || principal.Subject != "human" {
			t.Fatalf("token %q: principal = %+v, err = %v", token, principal, err)
		}
		if !fallback.called {
			t.Fatalf("token %q did not reach the fallback authenticator", token)
		}
	}
}

func TestMintValidatesInputs(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Mint("", 0); err == nil {
		t.Fatal("Mint with empty run ID must fail")
	}
	if _, err := registry.Mint("run-a", -time.Second); err == nil {
		t.Fatal("Mint with negative TTL must fail")
	}
}
