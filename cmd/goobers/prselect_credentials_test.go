package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestPRSelectExecutesLegacyAndModernCredentials(t *testing.T) {
	for _, authority := range []capability.Capability{capability.GitHubPRWrite, capability.ProviderPRWrite} {
		t.Run(string(authority), func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(42, "Select with the delivered credential")
			server.addOpenPR(42, "goobers/implementation/42", "main", "head-42", "base", false, nil, nil)
			// Do not use providerCmdEnv's legacy-to-modern test credential
			// injection: only the capability under test is delivered.
			providerCmdEnv(t, server, "", "pr-select-compatibility")
			setPRSelectTestCredentials(t, authority == capability.ProviderPRWrite, authority == capability.GitHubPRWrite)
			t.Chdir(t.TempDir())

			code, stdout, stderr := runArgs(t, "pr-select", root)
			if code != 0 || !strings.Contains(stdout, "selected PR #42") {
				t.Fatalf("pr-select: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			data, err := os.ReadFile("selected-pr.json")
			if err != nil {
				t.Fatal(err)
			}
			var selected map[string]string
			if err := decodePRSelectionTestResult(data, &selected); err != nil {
				t.Fatal(err)
			}
			if selected["number"] != "42" {
				t.Fatalf("selection = %s", data)
			}
		})
	}
}

func TestPRSelectCredentialRoutes(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderGitea, providers.ProviderADO} {
		for _, tc := range []struct {
			name           string
			modern, legacy bool
			wantToken      string
		}{
			{name: "legacy-only", legacy: true, wantToken: "legacy-token"},
			{name: "modern-only", modern: true, wantToken: "modern-token"},
			{name: "modern-precedence", modern: true, legacy: true, wantToken: "modern-token"},
			{name: "neither"},
		} {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				setPRSelectTestCredentials(t, tc.modern, tc.legacy)
				t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRMerge)), "not-a-write-credential")
				t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "not-a-pr-credential")
				requests := make(chan string, 10)
				root, repo := prSelectCredentialRoute(t, kind, func(w http.ResponseWriter, r *http.Request) {
					requests <- r.Header.Get("Authorization")
					writeEmptyPRSelectResponse(w, kind)
				})
				source, gate, err := newPRSelectSources(root, repo)
				if tc.wantToken == "" {
					if err == nil || !strings.Contains(err.Error(), "pr-select needs provider:pr:write (or github:pr:write)") {
						t.Fatalf("missing credentials: source=%T err=%v", source, err)
					}
					if len(requests) != 0 {
						t.Fatal("missing credentials reached the forge")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if (gate == nil) != (kind == providers.ProviderADO) {
					t.Fatalf("remediation gate = %T for %s", gate, kind)
				}
				_, _, err = source.pullRequests(t.Context(), repo, prSelectSourceRequest{base: "main"})
				if err != nil {
					t.Fatal(err)
				}
				want := "Bearer " + tc.wantToken
				if kind == providers.ProviderGitea {
					want = "token " + tc.wantToken
				}
				if len(requests) == 0 {
					t.Fatal("selection did not execute a provider request")
				}
				for len(requests) > 0 {
					if got := <-requests; got != want {
						t.Fatalf("authorization = %q, want %q", got, want)
					}
				}
			})
		}
	}
}

func TestPRSelectRefreshUsesSelectedAuthority(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		for _, authority := range []capability.Capability{capability.GitHubPRWrite, capability.ProviderPRWrite} {
			for _, mode := range []string{"expiry", "401", "revoked"} {
				t.Run(string(kind)+"/"+string(authority)+"/"+mode, func(t *testing.T) {
					resetStageRefreshingTokens(t)
					modern := authority == capability.ProviderPRWrite
					// A valid legacy credential must not mask a rejected modern grant.
					setPRSelectTestCredentials(t, modern, true)
					expiry := time.Now().Add(time.Hour)
					if mode == "expiry" {
						expiry = time.Now().Add(time.Minute)
					}
					t.Setenv(capability.CredentialExpiryEnvVar(string(authority)), expiry.Format(time.RFC3339))
					refreshes := make(chan string, 10)
					plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != apicontract.CredentialRefreshPath || r.Header.Get("Authorization") != "Bearer test-stage-grant" {
							t.Errorf("unexpected refresh request %s", r.URL.Path)
							http.Error(w, "wrong grant", http.StatusForbidden)
							return
						}
						var request struct {
							Capability string `json:"capability"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						refreshes <- request.Capability
						if mode == "revoked" {
							http.Error(w, `{"error":{"code":"credential_grant_revoked","message":"grant revoked"}}`, http.StatusForbidden)
							return
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"credentials": []map[string]any{{
							"capability": request.Capability, "value": "fresh-token", "expiresAt": time.Now().Add(time.Hour),
						}}})
					}))
					t.Cleanup(plane.Close)
					t.Setenv(executor.CredentialEndpointEnvVar, plane.URL)
					t.Setenv(executor.CredentialGrantEnvVar, "test-stage-grant")
					requests := make(chan string, 10)
					root, repo := prSelectCredentialRoute(t, kind, func(w http.ResponseWriter, r *http.Request) {
						authorization := r.Header.Get("Authorization")
						requests <- authorization
						if authorization != "Bearer fresh-token" {
							http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
							return
						}
						writeEmptyPRSelectResponse(w, kind)
					})
					source, _, err := newPRSelectSources(root, repo)
					if err != nil {
						t.Fatal(err)
					}
					_, _, err = source.pullRequests(t.Context(), repo, prSelectSourceRequest{base: "main"})
					if mode == "revoked" {
						if err == nil || !strings.Contains(err.Error(), "credential_grant_revoked") {
							t.Fatalf("revoked grant error = %v", err)
						}
						if len(requests) != 1 {
							t.Fatalf("revoked grant made %d forge requests, want only the initial rejected request", len(requests))
						}
						token := "legacy-token"
						if modern {
							token = "modern-token"
						}
						if got := <-requests; got != "Bearer "+token {
							t.Fatalf("revoked authority switched credential: authorization=%q", got)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						wantRequests := 1
						if mode == "401" {
							wantRequests = 2
						}
						if len(requests) != wantRequests {
							t.Fatalf("forge requests=%d, want %d", len(requests), wantRequests)
						}
						if mode == "401" {
							token := "legacy-token"
							if modern {
								token = "modern-token"
							}
							if got := <-requests; got != "Bearer "+token {
								t.Fatalf("first request = %q", got)
							}
						}
						if got := <-requests; got != "Bearer fresh-token" {
							t.Fatalf("refreshed request = %q", got)
						}
					}
					if len(refreshes) != 1 {
						t.Fatalf("refresh requests=%d, want 1", len(refreshes))
					}
					if got := <-refreshes; got != string(authority) {
						t.Fatalf("refreshed %q, want selected %q", got, authority)
					}
				})
			}
		}
	}
}

func TestPRSelectADOCredentialSchemeAndNoFallback(t *testing.T) {
	for _, authority := range []capability.Capability{capability.GitHubPRWrite, capability.ProviderPRWrite} {
		for _, scheme := range []string{"basic", "invalid"} {
			t.Run(string(authority)+"/"+scheme, func(t *testing.T) {
				modern := authority == capability.ProviderPRWrite
				setPRSelectTestCredentials(t, modern, true)
				t.Setenv(capability.CredentialSchemeEnvVar(string(authority)), scheme)
				token := "legacy-token"
				if modern {
					token = "modern-token"
				}
				requests := make(chan string, 10)
				root, repo := prSelectCredentialRoute(t, providers.ProviderADO, func(w http.ResponseWriter, r *http.Request) {
					requests <- r.Header.Get("Authorization")
					writeEmptyPRSelectResponse(w, providers.ProviderADO)
				})
				source, _, err := newPRSelectSources(root, repo)
				if scheme == "invalid" {
					if err == nil || !strings.Contains(err.Error(), capability.CredentialSchemeEnvVar(string(authority))) {
						t.Fatalf("invalid scheme error = %v", err)
					}
					if len(requests) != 0 {
						t.Fatal("invalid scheme reached the forge")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = source.pullRequests(t.Context(), repo, prSelectSourceRequest{base: "main"})
				if err != nil {
					t.Fatal(err)
				}
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:"+token))
				if len(requests) == 0 {
					t.Fatal("no ADO request")
				}
				for len(requests) > 0 {
					if got := <-requests; got != want {
						t.Fatalf("authorization = %q, want %q", got, want)
					}
				}
			})
		}
	}
}

func TestPRSelectAlternativeDoesNotAliasOtherCommandsOrLanding(t *testing.T) {
	setPRSelectTestCredentials(t, false, true)
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRMerge)), "")
	t.Setenv(executor.CredentialEnvVar(string(capability.ADOPRComplete)), "")
	for _, authority := range []capability.Capability{capability.ProviderPRWrite, capability.GitHubPRMerge, capability.ADOPRComplete} {
		if _, err := providerToken(authority); err == nil {
			t.Fatalf("legacy PR write unexpectedly authorized %s", authority)
		}
	}
	if _, err := landingAuthority(providers.RepositoryRef{Provider: providers.ProviderADO}); err == nil {
		t.Fatal("legacy PR write unexpectedly authorized ADO landing")
	}
}

func setPRSelectTestCredentials(t *testing.T, modern, legacy bool) {
	t.Helper()
	for _, credential := range []struct {
		capability capability.Capability
		delivered  bool
		token      string
	}{
		{capability.ProviderPRWrite, modern, "modern-token"},
		{capability.GitHubPRWrite, legacy, "legacy-token"},
	} {
		token := ""
		if credential.delivered {
			token = credential.token
		}
		t.Setenv(executor.CredentialEnvVar(string(credential.capability)), token)
		t.Setenv(capability.CredentialSchemeEnvVar(string(credential.capability)), "bearer")
		t.Setenv(capability.CredentialExpiryEnvVar(string(credential.capability)), "")
	}
	t.Setenv(executor.CredentialEndpointEnvVar, "")
	t.Setenv(executor.CredentialGrantEnvVar, "")
	t.Setenv(executor.GaggleEnvVar, "")
}

func prSelectCredentialRoute(t *testing.T, kind providers.ProviderKind, handler http.HandlerFunc) (string, providers.RepositoryRef) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	root := filepath.Join(t.TempDir(), "missing-instance")
	repo := providers.RepositoryRef{Provider: kind, Owner: "acme", Project: "project", Name: "widgets"}
	switch kind {
	case providers.ProviderGitHub:
		previous := newGitHubProvider
		t.Cleanup(func() { newGitHubProvider = previous })
		newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
			return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) {
				p.BaseURL = server.URL
			})...)
		}
	case providers.ProviderADO:
		previous := newADOProviderForStage
		t.Cleanup(func() { newADOProviderForStage = previous })
		newADOProviderForStage = func(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
			provider, err := buildADOProviderForStage(routed, credential)
			if err != nil {
				return nil, err
			}
			provider.BaseURL = server.URL
			return provider, nil
		}
	case providers.ProviderGitea:
		root, _ = giteaCounterInstance(t, server.URL)
	}
	return root, repo
}

func writeEmptyPRSelectResponse(w http.ResponseWriter, kind providers.ProviderKind) {
	w.Header().Set("Content-Type", "application/json")
	if kind == providers.ProviderADO {
		_, _ = w.Write([]byte(`{"value":[],"count":0}`))
	} else {
		_, _ = w.Write([]byte(`[]`))
	}
}
