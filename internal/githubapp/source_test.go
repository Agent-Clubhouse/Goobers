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
	"slices"
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
	// wantRepositories, when non-nil, asserts every mint request body
	// down-scopes the token to exactly these repository names.
	wantRepositories []string
}

func (f *fakeAppAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.requests.Add(1)
		if f.sleep > 0 {
			time.Sleep(f.sleep) // Intentional server latency exercises concurrent token request coalescing.
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
		if f.wantRepositories != nil {
			var mintReq struct {
				Repositories []string `json:"repositories"`
			}
			if err := json.NewDecoder(r.Body).Decode(&mintReq); err != nil {
				f.t.Errorf("decode mint request body: %v", err)
			} else if !slices.Equal(mintReq.Repositories, f.wantRepositories) {
				f.t.Errorf("mint body repositories = %v, want %v", mintReq.Repositories, f.wantRepositories)
			}
		}
		token := fmt.Sprintf("ghs_minted_%d", n)
		if f.nextToken != nil {
			token = f.nextToken(n)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, f.expiresAt().Format(time.RFC3339))
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

// TestTokenDownScopesToConfiguredRepositories pins the mint body: with
// Repositories configured, every exchange asks GitHub to scope the token to
// exactly those repos, so a shared App installation never yields a token
// reaching a sibling gaggle's repo.
func TestTokenDownScopesToConfiguredRepositories(t *testing.T) {
	api := &fakeAppAPI{t: t, key: appTestKey(t), appID: "123456", installationID: "42",
		expiresAt:        func() time.Time { return time.Now().Add(time.Hour) },
		wantRepositories: []string{"web"}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	source := newTokenSource(t, api, srv, func(c *Config) {
		c.Repositories = []string{"web"}
	})
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got := api.requests.Load(); got != 1 {
		t.Fatalf("exchanges = %d, want 1", got)
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
				_, _ = fmt.Fprint(w, tc.body)
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

// TestTokenRetriesTransientMintFailure is the #3792 regression: a transient
// 500 from GitHub's installation-token endpoint must not hard-fail the mint
// when a retry would have succeeded, matching the acceptance criteria's stub
// endpoint that returns 500 then 201.
func TestTokenRetriesTransientMintFailure(t *testing.T) {
	previousBase, previousMax := mintRetryBaseDelay, mintRetryMaxDelay
	mintRetryBaseDelay, mintRetryMaxDelay = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { mintRetryBaseDelay, mintRetryMaxDelay = previousBase, previousMax })

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"message":"Internal Server Error"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_after_retry","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	}))
	defer srv.Close()

	source, err := New(Config{AppID: "123456", InstallationID: "42", Key: staticKey(pkcs1PEM(appTestKey(t))), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: want the retry to succeed after the transient 500, got error: %v", err)
	}
	if token != "ghs_after_retry" {
		t.Fatalf("Token = %q, want ghs_after_retry", token)
	}
	if got := requests.Load(); got < 2 {
		t.Fatalf("requests = %d, want more than one HTTP attempt observed", got)
	}
}

// TestTokenDoesNotRetryTerminalMintFailures guards the other half of #3792:
// 401/403/404 are configuration faults a retry cannot fix, so the mint must
// give up after exactly one attempt and keep its existing actionable message.
func TestTokenDoesNotRetryTerminalMintFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"message":"bad JWT"}`, want: "auth.appId"},
		{name: "forbidden", status: http.StatusForbidden, body: `{"message":"suspended"}`, want: "permissions"},
		{name: "not found", status: http.StatusNotFound, body: `{"message":"Not Found"}`, want: "installation 42 not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			source, err := New(Config{AppID: "123456", InstallationID: "42", Key: staticKey(pkcs1PEM(appTestKey(t))), BaseURL: srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = source.Token(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Token error = %v, want a message containing %q", err, tc.want)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("requests = %d, want exactly 1 (terminal failures must not retry)", got)
			}
		})
	}
}

// TestTokenBoundsRetryAttemptsAndElapsedTime guards the "no unbounded retry
// loop" acceptance criterion: a persistently-failing endpoint must stop at
// mintMaxAttempts, in well under mintTimeout, and its terminal error must
// still name the app and installation.
func TestTokenBoundsRetryAttemptsAndElapsedTime(t *testing.T) {
	previousBase, previousMax := mintRetryBaseDelay, mintRetryMaxDelay
	mintRetryBaseDelay, mintRetryMaxDelay = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { mintRetryBaseDelay, mintRetryMaxDelay = previousBase, previousMax })

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"message":"Service Unavailable"}`)
	}))
	defer srv.Close()

	source, err := New(Config{AppID: "123456", InstallationID: "42", Key: staticKey(pkcs1PEM(appTestKey(t))), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	_, err = source.Token(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Token: want a terminal error for a persistently-failing endpoint, got nil")
	}
	for _, want := range []string{"app 123456", "installation 42", "status 503"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("terminal error %q does not mention %q", err, want)
		}
	}
	if got := requests.Load(); got != mintMaxAttempts {
		t.Fatalf("requests = %d, want exactly mintMaxAttempts (%d), no unbounded retry", got, mintMaxAttempts)
	}
	if elapsed > mintTimeout {
		t.Fatalf("elapsed = %s, want well under mintTimeout (%s)", elapsed, mintTimeout)
	}
}

// TestTokenRetryHonorsContextCancellation guards the context-deadline half of
// the "bounded" acceptance criterion: a caller-cancelled context must stop
// the retry loop promptly rather than spending its full attempt budget.
func TestTokenRetryHonorsContextCancellation(t *testing.T) {
	previousBase, previousMax := mintRetryBaseDelay, mintRetryMaxDelay
	mintRetryBaseDelay, mintRetryMaxDelay = 200*time.Millisecond, time.Second
	t.Cleanup(func() { mintRetryBaseDelay, mintRetryMaxDelay = previousBase, previousMax })

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"message":"Internal Server Error"}`)
	}))
	defer srv.Close()

	source, err := New(Config{AppID: "123456", InstallationID: "42", Key: staticKey(pkcs1PEM(appTestKey(t))), BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = source.Token(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Token: want an error once the context deadline is exceeded, got nil")
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want the retry loop to stop promptly once ctx is done", elapsed)
	}
	if got := requests.Load(); got >= mintMaxAttempts {
		t.Fatalf("requests = %d, want the context deadline to cut the loop short of the full attempt budget", got)
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
				_, _ = fmt.Fprint(w, tc.body)
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
		expiresAt: func() time.Time { return time.Now().Add(time.Hour) },
		// Source must down-scope mints to the one configured repo.
		wantRepositories: []string{"web"}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	repo := instance.RepoRef{Provider: "github", Owner: "acme", Name: "web",
		Auth: &instance.RepoAuthConfig{
			Kind:           instance.GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "42",
			PrivateKey:     &instance.TokenRef{File: keyPath},
		}}
	source, err := Source(repo, nil, nil)
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

func TestSourceFailsClosedOnStoreBackedKeyWithoutStores(t *testing.T) {
	repo := instance.RepoRef{Provider: "github", Owner: "acme", Name: "web",
		Auth: &instance.RepoAuthConfig{
			Kind:           instance.GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "42",
			PrivateKey:     &instance.TokenRef{Store: "prod-kv/app-key"},
		}}
	// A store-backed key with no store resolver must fail closed at
	// construction, never degrade into an unconfigured key (#683).
	_, err := Source(repo, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("Source error = %v, want store-ref fail-closed diagnosis", err)
	}
}

// fakeStoreResolver resolves store refs from a fixed map, standing in for the
// secretstore Registry so the App key can itself live in a secret store (#683).
type fakeStoreResolver map[string]string

func (f fakeStoreResolver) FetchSecret(_ context.Context, ref string) (string, error) {
	v, ok := f[ref]
	if !ok {
		return "", fmt.Errorf("fake store: no secret %q", ref)
	}
	return v, nil
}

func TestSourceResolvesStoreBackedKey(t *testing.T) {
	key := appTestKey(t)
	api := &fakeAppAPI{t: t, key: key, appID: "123456", installationID: "42",
		expiresAt:        func() time.Time { return time.Now().Add(time.Hour) },
		wantRepositories: []string{"web"}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	repo := instance.RepoRef{Provider: "github", Owner: "acme", Name: "web",
		Auth: &instance.RepoAuthConfig{
			Kind:           instance.GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "42",
			PrivateKey:     &instance.TokenRef{Store: "prod-kv/app-key"},
		}}
	stores := fakeStoreResolver{"prod-kv/app-key": pkcs1PEM(key)}
	source, err := Source(repo, nil, stores)
	if err != nil {
		t.Fatalf("Source: %v", err)
	}
	source.cfg.BaseURL = srv.URL
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token == "" {
		t.Fatal("Token: empty, want a minted installation token from a store-resolved key")
	}
}

func TestSourceRejectsNonAppRepo(t *testing.T) {
	repo := instance.RepoRef{Provider: "github", Owner: "acme", Name: "web",
		Token: instance.TokenRef{Env: "GH_TOKEN"}}
	if _, err := Source(repo, nil, nil); err == nil {
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

// TestTokenWithExpiryReportsTheMintedPairAtomically pins the DS10 primitive
// the credential plane rides: the expiry returned belongs to the exact value
// returned — served together from cache, and refreshed together on a
// near-expiry re-mint — so a mint response can state precisely how long the
// snapshot it hands a stage pod lives (#3489).
func TestTokenWithExpiryReportsTheMintedPairAtomically(t *testing.T) {
	base := time.Now()
	now := base
	api := &fakeAppAPI{t: t, key: appTestKey(t), appID: "123456", installationID: "42",
		expiresAt: func() time.Time { return now.Add(time.Hour) }}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	source := newTokenSource(t, api, srv, func(c *Config) {
		c.Now = func() time.Time { return now }
	})

	first, expiresAt, err := source.TokenWithExpiry(context.Background())
	if err != nil {
		t.Fatalf("TokenWithExpiry: %v", err)
	}
	if first == "" || expiresAt.IsZero() || !expiresAt.After(base) {
		t.Fatalf("first mint = %q expiry %v, want a stated future expiry", first, expiresAt)
	}
	// Cache hit: same value, same expiry.
	now = base.Add(30 * time.Minute)
	cached, cachedExpiry, err := source.TokenWithExpiry(context.Background())
	if err != nil || cached != first || !cachedExpiry.Equal(expiresAt) {
		t.Fatalf("cached = %q/%v err=%v, want %q/%v", cached, cachedExpiry, err, first, expiresAt)
	}
	// Near-expiry re-mint: fresh value, fresh later expiry, atomically.
	now = base.Add(56 * time.Minute)
	refreshed, refreshedExpiry, err := source.TokenWithExpiry(context.Background())
	if err != nil {
		t.Fatalf("TokenWithExpiry (refresh): %v", err)
	}
	if refreshed == first || !refreshedExpiry.After(cachedExpiry) {
		t.Fatalf("refresh = %q expiry %v, want a fresh token with a later expiry than %v", refreshed, refreshedExpiry, cachedExpiry)
	}
}
