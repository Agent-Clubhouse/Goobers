package oidcauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/goobers/goobers/internal/httpapi"
)

// Test keys are expensive to mint; share two across the package.
var (
	testKeysOnce sync.Once
	testKeyA     *rsa.PrivateKey
	testKeyB     *rsa.PrivateKey
)

func testKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	testKeysOnce.Do(func() {
		var err error
		if testKeyA, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if testKeyB, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return testKeyA, testKeyB
}

// fakeIssuer is an httptest-backed OIDC issuer: discovery document plus a
// mutable local JWKS endpoint.
type fakeIssuer struct {
	server *httptest.Server

	mu             sync.Mutex
	keys           map[string]*rsa.PrivateKey
	declaredIssuer string // overrides the discovery document's issuer when set
	jwksFetches    atomic.Int32
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	issuer := &fakeIssuer{keys: make(map[string]*rsa.PrivateKey)}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer.mu.Lock()
		declared := issuer.declaredIssuer
		issuer.mu.Unlock()
		if declared == "" {
			declared = issuer.server.URL
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   declared,
			"jwks_uri": issuer.server.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		issuer.jwksFetches.Add(1)
		issuer.mu.Lock()
		defer issuer.mu.Unlock()
		keys := make([]map[string]string, 0, len(issuer.keys))
		for kid, key := range issuer.keys {
			public := key.Public().(*rsa.PublicKey)
			keys = append(keys, map[string]string{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": kid,
				"n":   base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big31(public.E)),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)
	return issuer
}

func big31(e int) []byte {
	bytes := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
	for len(bytes) > 1 && bytes[0] == 0 {
		bytes = bytes[1:]
	}
	return bytes
}

func (f *fakeIssuer) URL() string { return f.server.URL }

func (f *fakeIssuer) addKey(kid string, key *rsa.PrivateKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[kid] = key
}

func (f *fakeIssuer) baseClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": f.URL(),
		"aud": "api://goobers",
		"sub": "user-1",
		"exp": time.Now().Add(10 * time.Minute).Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
	}
}

func mintToken(t *testing.T, key *rsa.PrivateKey, method jwt.SigningMethod, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	var signingKey any = key
	if _, hmac := method.(*jwt.SigningMethodHMAC); hmac {
		signingKey = []byte("shared-secret")
	}
	signed, err := token.SignedString(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func newTestAuthenticator(t *testing.T, issuer *fakeIssuer, mutate func(*Config)) *Authenticator {
	t.Helper()
	cfg := Config{
		Issuer:   issuer.URL(),
		Audience: "api://goobers",
		Roles: RoleMapping{
			View:    []string{"team-viewers"},
			Operate: []string{"team-operators"},
			Admin:   []string{"team-admins"},
		},
		HTTPClient: issuer.server.Client(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	authenticator, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return authenticator
}

func authenticate(t *testing.T, authenticator *Authenticator, token string) (*httpapi.Principal, error) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return authenticator.Authenticate(request)
}

func TestAuthenticateValidTokenMapsRolesAndPrincipal(t *testing.T) {
	keyA, _ := testKeys(t)
	issuer := newFakeIssuer(t)
	issuer.addKey("key-1", keyA)
	authenticator := newTestAuthenticator(t, issuer, nil)

	claims := issuer.baseClaims()
	claims["roles"] = []string{"team-viewers", "team-operators", "unmapped-group"}
	claims["preferred_username"] = "dev@example.com"
	principal, err := authenticate(t, authenticator, mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.Subject != "user-1" || principal.Issuer != issuer.URL() || principal.Name != "dev@example.com" {
		t.Fatalf("principal = %+v", principal)
	}
	if len(principal.Roles) != 2 || !principal.HasRole(httpapi.RoleOperate) || principal.HasRole(httpapi.RoleAdmin) {
		t.Fatalf("roles = %v", principal.Roles)
	}

	// A single-string roles claim maps the same way.
	claims = issuer.baseClaims()
	claims["roles"] = "team-admins"
	principal, err = authenticate(t, authenticator, mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !principal.HasRole(httpapi.RoleAdmin) {
		t.Fatalf("roles = %v", principal.Roles)
	}
}

// TestEntraShapedIssuerIsPureConfiguration exercises Entra-like configuration
// — GUID group claims under a "groups" claim — with zero issuer-specific
// code: the fake issuer simply serves that shape.
func TestEntraShapedIssuerIsPureConfiguration(t *testing.T) {
	keyA, _ := testKeys(t)
	issuer := newFakeIssuer(t)
	issuer.addKey("entra-kid", keyA)
	const operators = "22222222-aaaa-bbbb-cccc-000000000002"
	authenticator := newTestAuthenticator(t, issuer, func(cfg *Config) {
		cfg.RolesClaim = "groups"
		cfg.Roles = RoleMapping{
			View:    []string{"22222222-aaaa-bbbb-cccc-000000000001"},
			Operate: []string{operators},
		}
	})

	claims := issuer.baseClaims()
	claims["groups"] = []string{operators}
	claims["name"] = "Entra User"
	principal, err := authenticate(t, authenticator, mintToken(t, keyA, jwt.SigningMethodRS256, "entra-kid", claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.Name != "Entra User" || !principal.HasRole(httpapi.RoleOperate) || principal.HasRole(httpapi.RoleAdmin) {
		t.Fatalf("principal = %+v", principal)
	}
}

func TestAuthenticateRejectsInvalidTokens(t *testing.T) {
	keyA, keyB := testKeys(t)
	issuer := newFakeIssuer(t)
	issuer.addKey("key-1", keyA)
	authenticator := newTestAuthenticator(t, issuer, nil)

	expired := issuer.baseClaims()
	expired["exp"] = time.Now().Add(-10 * time.Minute).Unix()
	wrongAudience := issuer.baseClaims()
	wrongAudience["aud"] = "api://somebody-else"
	wrongIssuer := issuer.baseClaims()
	wrongIssuer["iss"] = "https://evil.example.com"
	noExpiry := issuer.baseClaims()
	delete(noExpiry, "exp")
	noSubject := issuer.baseClaims()
	delete(noSubject, "sub")

	tests := []struct {
		name  string
		token string
	}{
		{name: "no bearer token", token: ""},
		{name: "garbage token", token: "not-a-jwt-at-all"},
		{name: "expired", token: mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", expired)},
		{name: "wrong audience", token: mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", wrongAudience)},
		{name: "wrong issuer", token: mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", wrongIssuer)},
		{name: "missing expiry", token: mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", noExpiry)},
		{name: "missing subject", token: mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", noSubject)},
		{name: "signed by untrusted key", token: mintToken(t, keyB, jwt.SigningMethodRS256, "key-1", issuer.baseClaims())},
		{name: "hmac downgrade", token: mintToken(t, keyA, jwt.SigningMethodHS256, "key-1", issuer.baseClaims())},
		{name: "missing kid", token: mintToken(t, keyA, jwt.SigningMethodRS256, "", issuer.baseClaims())},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			principal, err := authenticate(t, authenticator, test.token)
			if err == nil {
				t.Fatalf("Authenticate accepted %s token: %+v", test.name, principal)
			}
			if strings.Contains(err.Error(), test.token) && test.token != "" {
				t.Fatalf("error leaks token material: %v", err)
			}
		})
	}
}

func TestJWKSRefreshOnRotationIsRateLimited(t *testing.T) {
	keyA, keyB := testKeys(t)
	issuer := newFakeIssuer(t)
	issuer.addKey("key-1", keyA)
	authenticator := newTestAuthenticator(t, issuer, nil)

	if _, err := authenticate(t, authenticator, mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", issuer.baseClaims())); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	fetches := issuer.jwksFetches.Load()

	// A rotated key's kid is unknown; within the refresh interval the request
	// fails without hammering the issuer.
	rotated := mintToken(t, keyB, jwt.SigningMethodRS256, "key-2", issuer.baseClaims())
	issuer.addKey("key-2", keyB)
	if _, err := authenticate(t, authenticator, rotated); err == nil {
		t.Fatal("expected unknown kid inside the refresh interval to fail")
	}
	if got := issuer.jwksFetches.Load(); got != fetches {
		t.Fatalf("JWKS fetched %d times, want %d (rate limited)", got, fetches)
	}

	// Once the interval elapses the unknown kid triggers one refetch and the
	// rotated key validates.
	authenticator.mu.Lock()
	authenticator.lastRefresh = time.Now().Add(-2 * authenticator.refreshInterval)
	authenticator.mu.Unlock()
	if _, err := authenticate(t, authenticator, rotated); err != nil {
		t.Fatalf("Authenticate after rotation: %v", err)
	}
	if got := issuer.jwksFetches.Load(); got != fetches+1 {
		t.Fatalf("JWKS fetched %d times, want %d", got, fetches+1)
	}
}

func TestDiscoveryIssuerMismatchFailsClosed(t *testing.T) {
	keyA, _ := testKeys(t)
	issuer := newFakeIssuer(t)
	issuer.addKey("key-1", keyA)
	issuer.mu.Lock()
	issuer.declaredIssuer = "https://impostor.example.com"
	issuer.mu.Unlock()
	authenticator := newTestAuthenticator(t, issuer, nil)

	_, err := authenticate(t, authenticator, mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", issuer.baseClaims()))
	if err == nil || !strings.Contains(err.Error(), "declares issuer") {
		t.Fatalf("Authenticate = %v, want discovery issuer mismatch", err)
	}
}

func TestUnmappedPrincipalGetsNoRoles(t *testing.T) {
	keyA, _ := testKeys(t)
	issuer := newFakeIssuer(t)
	issuer.addKey("key-1", keyA)
	authenticator := newTestAuthenticator(t, issuer, nil)

	claims := issuer.baseClaims()
	claims["roles"] = []string{"some-unrelated-group"}
	principal, err := authenticate(t, authenticator, mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(principal.Roles) != 0 {
		t.Fatalf("roles = %v, want none (deny by default)", principal.Roles)
	}

	noClaim := issuer.baseClaims()
	principal, err = authenticate(t, authenticator, mintToken(t, keyA, jwt.SigningMethodRS256, "key-1", noClaim))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(principal.Roles) != 0 {
		t.Fatalf("roles = %v, want none", principal.Roles)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "issuer required", cfg: Config{Audience: "a"}, wantErr: "issuer is required"},
		{name: "issuer must be absolute", cfg: Config{Issuer: "issuer.example.com", Audience: "a"}, wantErr: "absolute http(s) URL"},
		{name: "audience required", cfg: Config{Issuer: "https://issuer.example.com"}, wantErr: "audience is required"},
		{
			name: "empty mapped claim value rejected",
			cfg: Config{
				Issuer:   "https://issuer.example.com",
				Audience: "a",
				Roles:    RoleMapping{View: []string{""}},
			},
			wantErr: "empty claim values",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.cfg); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("New() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
