package providers_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/githubapp"
	"github.com/goobers/goobers/providers"
)

// TestPreflightRepositoryWriteCredentialMatrix is the regression coverage a
// prior attempt at #4414 was reviewed as lacking: every case there called
// NewGitHubProvider("credential", ...) — the credential field was declared
// but never used, so the three named cases exercised one identical code
// path under three labels. This matrix resolves each credential through the
// SAME mechanism `goobers validate`/push-branch actually use — a static
// TokenRef for a PAT, and a real githubapp.TokenSource mint for a GitHub App
// installation token — and proves PreflightRepositoryWrite's outgoing
// request actually carries THAT case's distinct, independently resolved
// value: a server that only accepts a different token, or a value from a
// different case, fails the request exactly like a real misconfigured
// credential would.
//
// GitHub's own API does not expose a machine-distinguishable "fine-grained
// vs. classic" PAT signal (both are opaque bearer tokens; see
// internal/credentials, which models exactly one PAT shape), so the two
// static-token cases are resolved from two independent credentials.TokenRef
// env sources with distinct values — proving real, separate resolution
// rather than a shared stand-in — while the GitHub App case is the
// genuinely different code path: a minted, ephemeral installation token via
// internal/githubapp.
func TestPreflightRepositoryWriteCredentialMatrix(t *testing.T) {
	const (
		classicTokenEnv       = "GOOBERS_TEST_CLASSIC_PAT"
		classicTokenValue     = "classic-pat-token-value"
		fineGrainedTokenEnv   = "GOOBERS_TEST_FINEGRAINED_PAT"
		fineGrainedTokenValue = "fine-grained-pat-token-value"
	)
	t.Setenv(classicTokenEnv, classicTokenValue)
	t.Setenv(fineGrainedTokenEnv, fineGrainedTokenValue)

	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "classic", Env: classicTokenEnv},
		{Name: "fine-grained", Env: fineGrainedTokenEnv},
	})
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	classicToken, err := resolver.Resolve(context.Background(), "classic")
	if err != nil {
		t.Fatalf("resolve classic token: %v", err)
	}
	fineGrainedToken, err := resolver.Resolve(context.Background(), "fine-grained")
	if err != nil {
		t.Fatalf("resolve fine-grained token: %v", err)
	}
	if classicToken == fineGrainedToken {
		t.Fatalf("classic and fine-grained tokens resolved to the same value %q — cases are not distinct", classicToken)
	}

	appTokenValue := "github-app-installation-token-value"
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test RSA key: %v", err)
	}
	appKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey)})

	cases := []struct {
		name          string
		wantToken     string
		buildProvider func(t *testing.T, serverURL string) *providers.GitHubProvider
	}{
		{
			name:      "classic token",
			wantToken: classicToken,
			buildProvider: func(t *testing.T, serverURL string) *providers.GitHubProvider {
				return providers.NewGitHubProvider(classicToken, func(p *providers.GitHubProvider) { p.BaseURL = serverURL })
			},
		},
		{
			name:      "fine-grained PAT",
			wantToken: fineGrainedToken,
			buildProvider: func(t *testing.T, serverURL string) *providers.GitHubProvider {
				return providers.NewGitHubProvider(fineGrainedToken, func(p *providers.GitHubProvider) { p.BaseURL = serverURL })
			},
		},
		{
			name:      "GitHub App installation token",
			wantToken: appTokenValue,
			buildProvider: func(t *testing.T, serverURL string) *providers.GitHubProvider {
				source, err := githubapp.New(githubapp.Config{
					AppID:          "12345",
					InstallationID: "67890",
					Key:            func(context.Context) (string, error) { return string(appKeyPEM), nil },
					BaseURL:        serverURL,
					Now:            time.Now,
				})
				if err != nil {
					t.Fatalf("build githubapp source: %v", err)
				}
				return providers.NewGitHubProvider("", func(p *providers.GitHubProvider) { p.BaseURL = serverURL }, providers.WithTokenSource(source))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/app/installations/67890/access_tokens", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"token":      appTokenValue,
					"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				})
			})
			mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "token "+tc.wantToken && got != "Bearer "+tc.wantToken {
					http.Error(w, "unauthorized: unexpected credential", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"permissions": map[string]interface{}{"push": true},
				})
			})
			mux.HandleFunc("/repos/acme/app/rules/branches/goobers/run-1", func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "token "+tc.wantToken && got != "Bearer "+tc.wantToken {
					http.Error(w, "unauthorized: unexpected credential", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := tc.buildProvider(t, server.URL)
			result, err := provider.PreflightRepositoryWrite(context.Background(),
				providers.RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
			if err != nil {
				t.Fatalf("PreflightRepositoryWrite returned error: %v", err)
			}
			if !result.OK {
				t.Fatalf("result = %+v, want OK — the server only accepts this case's own distinct credential, so a non-OK result means the wrong token was sent", result)
			}
		})
	}
}

// TestPreflightRepositoryWriteCredentialMatrixCatchesWrongCredential closes
// the loop on the matrix above: it proves the check would actually FAIL a
// case that sent the wrong credential, so a shared-stand-in regression (all
// cases secretly using one token) could not pass the matrix silently.
func TestPreflightRepositoryWriteCredentialMatrixCatchesWrongCredential(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer expected-token" {
			http.Error(w, "unauthorized: unexpected credential", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"permissions": map[string]interface{}{"push": true}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := providers.NewGitHubProvider("wrong-token", func(p *providers.GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.PreflightRepositoryWrite(context.Background(),
		providers.RepositoryRef{Owner: "acme", Name: "app"}, "goobers/run-1")
	if err != nil {
		t.Fatalf("PreflightRepositoryWrite returned error: %v", err)
	}
	if result.OK || result.FailureCapability != providers.RepoWriteFailureUnauthorized {
		t.Fatalf("result = %+v, want FailureCapability %q for the wrong credential", result, providers.RepoWriteFailureUnauthorized)
	}
}
