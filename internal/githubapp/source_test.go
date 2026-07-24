package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/goobers/goobers/internal/instance"
)

// testKey is generated once: RSA keygen dominates the suite's wall clock and
// every test can share one keypair without loss of isolation.
var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

func appTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}
		testKey = key
	})
	return testKey
}

func pkcs1PEM(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func pkcs8PEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func staticKey(pemStr string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return pemStr, nil }
}

type spyRegistrar struct {
	mu     sync.Mutex
	values []string
}

func (s *spyRegistrar) Register(secret []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = append(s.values, string(secret))
}

func (s *spyRegistrar) saw(value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.values {
		if v == value {
			return true
		}
	}
	return false
}

// fakeAppAPI is an httptest GitHub App token-exchange endpoint. Each request's
// App JWT is verified against the test key before a token is issued.
type fakeAppAPI struct {
	t              *testing.T
	key            *rsa.PrivateKey
	appID          string
	installationID string
	expiresAt      func() time.Time
	requests       atomic.Int64
	sleep          time.Duration
	nextToken      func(n int64) string
}

func (f *fakeAppAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.requests.Add(1)
		if f.sleep > 0 {
			time.Sleep(f.sleep)
		}
		wantPath := "/app/installations/" + f.installationID + "/access_tokens"
		if r.Method != http.MethodPost || r.URL.Path != wantPath {
			f.t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, wantPath)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			f.t.Errorf("Authorization = %q, want Bearer App JWT", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		claims := &jwt.RegisteredClaims{}
		if _, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (interface{}, error) {
			return &f.key.PublicKey, nil
		}, jwt.WithValidMethods([]string{"RS256"})); err != nil {
			f.t.Errorf("App JWT does not verify as RS256 with the App key: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if claims.Issuer != f.appID {
			f.t.Errorf("App JWT iss = %q, want %q", claims.Issuer, f.appID)
		}
		if claims.IssuedAt == nil || claims.ExpiresAt == nil {
			f.t.Error("App JWT must carry iat and exp")
		} else if lifetime := claims.ExpiresAt.Sub(claims.IssuedAt.Time); lifetime > 10*time.Minute {
			// GitHub rejects JWTs whose validity window exceeds 10 minutes;
			// measured iat→exp so the assertion also holds under fake clocks.
			f.t.Errorf("App JWT exp-iat = %s, want <= 10m (GitHub's cap)", lifetime)
		}
		token := fmt.Sprintf("ghs_minted_%d", n)
		if f.nextToken != nil {
			token = f.nextToken(n)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, f.expiresAt().Format(time.RFC3339))
	})
}

func newTokenSource(t *testing.T, api *fakeAppAPI, srv *httptest.Server, mutate func(*Config)) *TokenSource {
	t.Helper()
	cfg := Config{
		AppID:          api.appID,
		InstallationID: api.installationID,
		Key:            staticKey(pkcs1PEM(api.key)),
		BaseURL:        srv.URL,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	source, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return source
}

func TestTokenMintsVerifiableAppJWT(t *testing.T) {
	key := appTestKey(t)
	api := &fakeAppAPI{t: t, key: key, appID: "123456", installationID: "42",
		expiresAt: func() time.Time { return time.Now().Add(time.Hour) }}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	reg := &spyRegistrar{}
	source := newTokenSource(t, api, srv, func(c *Config) { c.Registrar = reg })

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "ghs_minted_1" {
		t.Fatalf("Token = %q, want ghs_minted_1", token)
	}
	if !reg.saw(token) {
		t.Fatal("minted token was not registered with the scrubber")
	}
	if !reg.saw(pkcs1PEM(key)) {
		t.Fatal("App private key was not registered with the scrubber")
	}
}

func TestTokenCachesUntilNearExpiry(t *testing.T) {
	base := time.Now()
	now := base
	api := &fakeAppAPI{t: t, key: appTestKey(t), appID: "123456", installationID: "42",
		expiresAt: func() time.Time { return base.Add(time.Hour) }}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	source := newTokenSource(t, api, srv, func(c *Config) {
		c.Now = func() time.Time { return now }
	})

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	// Well inside the window: served from cache, no second exchange.
	now = base.Add(30 * time.Minute)
	cached, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (cached): %v", err)
	}
	if cached != first {
		t.Fatalf("cached token = %q, want %q", cached, first)
	}
	if got := api.requests.Load(); got != 1 {
		t.Fatalf("exchanges = %d, want 1 (cache hit)", got)
	}
	// Inside the 5m refresh skew of the 60m expiry: re-mint.
	now = base.Add(56 * time.Minute)
	refreshed, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (refresh): %v", err)
	}
	if refreshed == first {
		t.Fatalf("token near expiry = %q, want a fresh mint", refreshed)
	}
	if got := api.requests.Load(); got != 2 {
		t.Fatalf("exchanges = %d, want 2 (near-expiry re-mint)", got)
	}
}

func TestTokenSingleFlightsConcurrentMints(t *testing.T) {
	api := &fakeAppAPI{t: t, key: appTestKey(t), appID: "123456", installationID: "42",
		expiresAt: func() time.Time { return time.Now().Add(time.Hour) },
		sleep:     50 * time.Millisecond}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	source := newTokenSource(t, api, srv, nil)

	const callers = 8
	tokens := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = source.Token(context.Background())
		}(i)
	}
	wg.Wait()
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("Token[%d]: %v", i, errs[i])
		}
		if tokens[i] != tokens[0] {
			t.Fatalf("Token[%d] = %q, want the single-flighted %q", i, tokens[i], tokens[0])
		}
	}
	if got := api.requests.Load(); got != 1 {
		t.Fatalf("exchanges = %d, want 1 (no mint stampede)", got)
	}
}

func TestTokenAcceptsPKCS8Key(t *testing.T) {
	key := appTestKey(t)
	api := &fakeAppAPI{t: t, key: key, appID: "123456", installationID: "42",
		expiresAt: func() time.Time { return time.Now().Add(time.Hour) }}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	source := newTokenSource(t, api, srv, func(c *Config) {
		c.Key = staticKey(pkcs8PEM(t, key))
	})
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("Token with PKCS8 key: %v", err)
	}
}

func TestTokenMintErrorsAreActionableAndSecretFree(t *testing.T) {
	keyPEM := pkcs1PEM(appTestKey(t))
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{
			name:   "rejected JWT",
			status: http.StatusUnauthorized,
			body:   `{"message":"A JSON web token could not be decoded"}`,
			want:   []string{"auth.appId", "auth.privateKey", "A JSON web token could not be decoded"},
		},
		{
			name:   "app not installed",
			status: http.StatusNotFound,
			body:   `{"message":"Not Found"}`,
			want:   []string{"installation 42 not found", "install the App"},
		},
		{
			name:   "permissions refused",
			status: http.StatusForbidden,
			body:   `{"message":"This installation has been suspended"}`,
			want:   []string{"permissions", "suspended"},
		},
		{
			name:   "opaque body never echoed",
			status: http.StatusBadGateway,
			body:   `pseudo-secret-body-content`,
			want:   []string{"status 502", "(no error message)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			source, err := New(Config{AppID: "123456", InstallationID: "42", Key: staticKey(keyPEM), BaseURL: srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = source.Token(context.Background())
			if err == nil {
				t.Fatal("Token: want mint error, got nil")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
			for _, secret := range []string{keyPEM, "eyJ", "pseudo-secret-body-content"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks secret material: %q", err)
				}
			}
		})
	}
}

func TestTokenRejectsMalformedMintResponses(t *testing.T) {
	keyPEM := pkcs1PEM(appTestKey(t))
	cases := []struct {
		name string
		body string
	}{
		{name: "empty token", body: `{"token":"","expires_at":"2030-01-01T00:00:00Z"}`},
		{name: "missing expiry", body: `{"token":"ghs_x"}`},
		{name: "already expired", body: `{"token":"ghs_x","expires_at":"2001-01-01T00:00:00Z"}`},
		{name: "not JSON", body: `<!doctype html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			source, err := New(Config{AppID: "123456", InstallationID: "42", Key: staticKey(keyPEM), BaseURL: srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := source.Token(context.Background()); err == nil {
				t.Fatal("Token: want error for malformed response, got nil")
			}
		})
	}
}

func TestTokenFailsClosedOnUnparseableKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("exchange must not be attempted with an unparseable key")
	}))
	defer srv.Close()
	source, err := New(Config{AppID: "123456", InstallationID: "42",
		Key: staticKey("not a pem key"), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = source.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("Token error = %v, want PEM diagnosis", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	key := staticKey(pkcs1PEM(appTestKey(t)))
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "missing app ID", cfg: Config{InstallationID: "42", Key: key}, want: "app ID is required"},
		{name: "missing installation ID", cfg: Config{AppID: "1", Key: key}, want: "installation ID is required"},
		{name: "missing key", cfg: Config{AppID: "1", InstallationID: "42"}, want: "private key source is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSourceBuildsFromInstanceRepo(t *testing.T) {
	key := appTestKey(t)
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, []byte(pkcs1PEM(key)), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	api := &fakeAppAPI{t: t, key: key, appID: "123456", installationID: "42",
		expiresAt: func() time.Time { return time.Now().Add(time.Hour) }}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	repo := instance.RepoRef{Provider: "github", Owner: "acme", Name: "web",
		Auth: &instance.RepoAuthConfig{
			Kind:           instance.GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "42",
			PrivateKey:     &instance.TokenRef{File: keyPath},
		}}
	source, err := Source(repo, nil)
	if err != nil {
		t.Fatalf("Source: %v", err)
	}
	// Point the assembled source at the fake exchange endpoint.
	source.cfg.BaseURL = srv.URL
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "ghs_minted_1" {
		t.Fatalf("Token = %q, want ghs_minted_1", token)
	}
}

func TestSourceFailsClosedOnStoreBackedKey(t *testing.T) {
	repo := instance.RepoRef{Provider: "github", Owner: "acme", Name: "web",
		Auth: &instance.RepoAuthConfig{
			Kind:           instance.GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "42",
			PrivateKey:     &instance.TokenRef{Store: "prod-kv/app-key"},
		}}
	_, err := Source(repo, nil)
	if err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("Source error = %v, want store-ref fail-closed diagnosis", err)
	}
}

func TestSourceRejectsNonAppRepo(t *testing.T) {
	repo := instance.RepoRef{Provider: "github", Owner: "acme", Name: "web",
		Token: instance.TokenRef{Env: "GH_TOKEN"}}
	if _, err := Source(repo, nil); err == nil {
		t.Fatal("Source: want error for a PAT repo, got nil")
	}
}

// TestTokenRoundTripsExpiryEncoding pins the JSON wire shape: GitHub returns
// RFC3339 expires_at; a parse regression would silently break caching.
func TestTokenRoundTripsExpiryEncoding(t *testing.T) {
	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(`{"token":"ghs_x","expires_at":"2030-06-01T12:00:00Z"}`), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.ExpiresAt != time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC) {
		t.Fatalf("expires_at = %v", payload.ExpiresAt)
	}
}
