package oidcauth

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/readservice"
)

// healthOnlyReader satisfies readservice.Reader for the single route this
// test exercises; every other method panics via the embedded nil interface.
type healthOnlyReader struct{ readservice.Reader }

func (healthOnlyReader) Health(context.Context) (readservice.Health, error) {
	return readservice.Health{Ready: true}, nil
}

// TestOIDCAuthenticatedAPIEndToEnd drives the real handler stack — OIDC
// authenticator, role-floor authorizer, versioned routes — with tokens from
// the fake issuer: the composition `goobers up` wires when api.auth.oidc is
// configured.
func TestOIDCAuthenticatedAPIEndToEnd(t *testing.T) {
	keyA, _ := testKeys(t)
	issuer := newFakeIssuer(t)
	issuer.addKey("key-1", keyA)
	authenticator := newTestAuthenticator(t, issuer, nil)

	handler, err := httpapi.NewHandler(
		healthOnlyReader{},
		httpapi.RequireRoles(),
		log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	get := func(t *testing.T, token string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, server.URL+httpapi.HealthPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}

	viewClaims := issuer.baseClaims()
	viewClaims["roles"] = []string{"team-viewers"}
	response := get(t, mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", viewClaims))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("view token status = %d", response.StatusCode)
	}
	var health readservice.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if !health.Ready {
		t.Fatalf("health = %+v", health)
	}

	if response := get(t, ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", response.StatusCode)
	}

	expired := issuer.baseClaims()
	expired["roles"] = []string{"team-viewers"}
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	if response := get(t, mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", expired)); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want 401", response.StatusCode)
	}

	unmapped := issuer.baseClaims()
	unmapped["roles"] = []string{"not-a-goobers-group"}
	if response := get(t, mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", unmapped)); response.StatusCode != http.StatusForbidden {
		t.Fatalf("unmapped token status = %d, want 403", response.StatusCode)
	}
}
