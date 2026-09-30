package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

type appDeliveryTestTransport func(*http.Request) (*http.Response, error)

func (f appDeliveryTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// fakeAppMintAPI answers GitHub App installation-token exchanges on the
// default transport. The first token it issues has less than
// credentials.MinDeliveredLifetime left (but more than the source cache's
// own refresh skew); every later one lives an hour.
type fakeAppMintAPI struct {
	mu    sync.Mutex
	mints int
}

func (a *fakeAppMintAPI) install(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = appDeliveryTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || !strings.HasPrefix(req.URL.Path, "/app/installations/") || !strings.HasSuffix(req.URL.Path, "/access_tokens") {
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL)
		}
		a.mu.Lock()
		a.mints++
		n := a.mints
		a.mu.Unlock()
		lifetime := time.Hour
		if n == 1 {
			lifetime = credentials.MinDeliveredLifetime / 2
		}
		body, err := json.Marshal(map[string]any{
			"token":      fmt.Sprintf("app-installation-token-%d-0123456789", n),
			"expires_at": time.Now().Add(lifetime).UTC().Format(time.RFC3339),
		})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Request:    req,
		}, nil
	})
}

func (a *fakeAppMintAPI) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mints
}

// setAppDeliveryTestKey generates an App private key and exposes it through
// an environment variable, returning the key's token ref.
func setAppDeliveryTestKey(t *testing.T) *instance.TokenRef {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const env = "GOOBERS_TEST_APP_DELIVERY_FLOOR_KEY"
	t.Setenv(env, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	return &instance.TokenRef{Env: env}
}

// TestGitHubAppTokenSourcesApplyTheDeliveryFloor pins the #5905 wiring for
// every GitHub App source the daemon delivers to stages (a repository's App,
// an agent:model App grant, the daemon identity): a freshly minted token with
// less than credentials.MinDeliveredLifetime left is re-minted before it is
// returned, instead of being handed to a stage that cannot refresh it.
func TestGitHubAppTokenSourcesApplyTheDeliveryFloor(t *testing.T) {
	keyRef := setAppDeliveryTestKey(t)
	for _, tc := range []struct {
		name  string
		build func() (credentials.ExpiringResolveFunc, error)
	}{
		{
			name: "repository",
			build: func() (credentials.ExpiringResolveFunc, error) {
				return newGitHubAppTokenSource(instance.RepoRef{
					Provider: "github", Owner: "example-org", Name: "example-repo",
					Auth: &instance.RepoAuthConfig{Kind: instance.GitHubAuthApp, AppID: "123456", InstallationID: "42", PrivateKey: keyRef},
				}, nil, nil)
			},
		},
		{
			name: "agent:model",
			build: func() (credentials.ExpiringResolveFunc, error) {
				return newAgentModelGitHubAppTokenSource(&instance.AgentModelGitHubAppConfig{
					Name: "example-model-app", AppID: "123456", InstallationID: "42",
					Repository: "example-org/example-repo", RepositoryID: "987654", PrivateKey: keyRef,
				}, nil, nil)
			},
		},
		{
			name: "daemon identity",
			build: func() (credentials.ExpiringResolveFunc, error) {
				return newDaemonIdentityGitHubAppTokenSource(&instance.DaemonIdentityConfig{
					Kind: instance.GitHubAuthApp, AppID: "123456", InstallationID: "42", PrivateKey: keyRef,
				}, "example-org", "example-repo", nil, nil)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAppMintAPI{}
			api.install(t)
			mint, err := tc.build()
			if err != nil {
				t.Fatalf("build source: %v", err)
			}
			token, expiresAt, err := mint(context.Background())
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if token != "app-installation-token-2-0123456789" {
				t.Fatalf("delivered %q, want the re-minted token", token)
			}
			if remaining := time.Until(expiresAt); remaining < credentials.MinDeliveredLifetime {
				t.Fatalf("delivered token has %v left, want at least %v", remaining, credentials.MinDeliveredLifetime)
			}
			if got := api.count(); got != 2 {
				t.Fatalf("mints = %d, want 2 (the short-lived mint and its re-mint)", got)
			}
		})
	}
}
