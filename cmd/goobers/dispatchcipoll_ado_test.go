package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// Both servers are local protocol fixtures. The provider factory still receives
// the real typed broker source and uses the production ADO HTTP implementation.
func setupADOPodPoll(t *testing.T, scheme string, resolve func(int) dispatcher.MintedCredential, ado http.HandlerFunc) *int {
	t.Helper()
	setPodCIPollEnv(t, defaultCIPollFixture())
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderADO))
	t.Setenv(executor.RepoProjectEnvVar, "project")
	t.Setenv(executor.GaggleEnvVar, "gaggle")
	// A GitHub backlog must not make this PR credential look like a backlog
	// credential at the registered stage-provider safety gate.
	t.Setenv(dispatcher.GaggleBacklogEnv, `{"provider":"github","project":"acme/backlog"}`)
	t.Setenv(executor.RepoAuthSchemeEnvVar, "ambient-must-not-be-used")
	calls := new(int)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apicontract.CredentialResolvePath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		*calls++
		var req dispatcher.CredentialResolveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.RunID != "run-ci-poll" || req.Stage != "ci-poll" || len(req.Capabilities) != 1 || req.Capabilities[0] != string(capability.ProviderPRWrite) || r.Header.Get("Authorization") != "Bearer pod-token" {
			t.Errorf("wrong broker authority/request: %+v", req)
		}
		cred := resolve(*calls)
		cred.Capability = string(capability.ProviderPRWrite)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"credentials": []dispatcher.MintedCredential{cred}, "repoAuthScheme": scheme})
	}))
	t.Cleanup(broker.Close)
	t.Setenv(dispatcher.EnvDaemonAPI, broker.URL)
	providerServer := httptest.NewServer(ado)
	t.Cleanup(providerServer.Close)
	previous := newADOProviderForStage
	newADOProviderForStage = func(repo providers.RepositoryRef, source providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(repo.Owner, repo.Project, "", providers.WithADOCredentialSource(source), providers.WithADOMaxRateLimitRetries(0), func(p *providers.ADOProvider) { p.BaseURL = providerServer.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = previous })
	return calls
}

func adoPodPR(status string) map[string]interface{} {
	return map[string]interface{}{"pullRequestId": 41, "status": status, "sourceRefName": "refs/heads/topic", "targetRefName": "refs/heads/main", "repository": map[string]interface{}{"id": "repo-id", "name": "web", "project": map[string]string{"id": "project-id", "name": "project"}}}
}

func TestCIPollADOPodNativePolicyAndLifecycle(t *testing.T) {
	for _, tc := range []struct{ name, status, policy, want string }{
		{"pending then passing", "active", "running", string(providers.CheckStatePassing)},
		{"passing", "active", "approved", string(providers.CheckStatePassing)},
		{"failing", "active", "rejected", string(providers.CheckStateFailing)},
		{"zero policy", "active", "", string(providers.CheckStatePassing)},
		{"abandoned", "abandoned", "running", executor.CIStatusClosed},
		{"merged", "completed", "running", executor.CIStatusMerged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policies := 0
			setupADOPodPoll(t, "bearer", func(int) dispatcher.MintedCredential { return dispatcher.MintedCredential{Value: "delivered"} }, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer delivered" {
					t.Errorf("wrong auth %q", r.Header.Get("Authorization"))
				}
				if strings.HasSuffix(r.URL.Path, "/pullrequests/41") {
					_ = json.NewEncoder(w).Encode(adoPodPR(tc.status))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/policy/evaluations") {
					policies++
					if r.URL.Query().Get("artifactId") != "vstfs:///CodeReview/CodeReviewId/project-id/41" {
						t.Error("wrong policy identity")
					}
					value := []map[string]interface{}{}
					if tc.policy != "" {
						status := tc.policy
						if status == "running" && policies > 1 {
							status = "approved"
						}
						value = append(value, map[string]interface{}{"status": status, "configuration": map[string]interface{}{"isEnabled": true, "isBlocking": true, "type": map[string]string{"displayName": "Build"}}})
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"value": value})
					return
				}
				w.WriteHeader(http.StatusNotFound)
			})
			result := runCIPollStage(context.Background(), io.Discard)
			wantStatus := apiv1.ResultSuccess
			if tc.status == "abandoned" {
				wantStatus = apiv1.ResultFailure
			}
			if result.Status != wantStatus || result.Outputs[executor.OutputCIStatus] != tc.want {
				t.Fatalf("result=%+v", result)
			}

			if tc.name == "pending then passing" && policies != 2 {
				t.Fatalf("policy polls=%d", policies)
			}
			if tc.name == "failing" && len(result.Artifacts) == 0 {
				t.Fatal("missing CI evidence")
			}
		})
	}
}

func TestCIPollADOPodRefreshesOnceAndScrubsRotatedBasic(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotate", true: "scheme changes"}[mismatch], func(t *testing.T) {
			requests := 0
			fresh := "rotated-pat-secret"
			encoded := base64.StdEncoding.EncodeToString([]byte("goobers:" + fresh))
			calls := setupADOPodPoll(t, "bearer", func(n int) dispatcher.MintedCredential {
				if n == 1 {
					return dispatcher.MintedCredential{Value: "initial-pat", Scheme: "basic"}
				}
				scheme := "basic"
				if mismatch {
					scheme = "bearer"
				}
				return dispatcher.MintedCredential{Value: fresh, Scheme: scheme}
			}, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "Basic "+encoded {
					t.Error("rotation did not use Basic")
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, fresh+" "+encoded)
			})
			result := runCIPollStage(context.Background(), io.Discard)
			if result.Error == nil {
				t.Fatalf("expected auth failure: %+v", result)
			}
			if *calls != 2 {
				t.Fatalf("broker resolves=%d", *calls)
			}
			want := 2
			if mismatch {
				want = 1
			}
			if requests != want {
				t.Fatalf("requests=%d want=%d", requests, want)
			}
			data, _ := json.Marshal(result)
			if strings.Contains(string(data), fresh) || strings.Contains(string(data), encoded) {
				t.Fatalf("credential leaked: %s", data)
			}
		})
	}
}

func TestCIPollADOPodRefusesInvalidDeliveryAndScope(t *testing.T) {
	expired := time.Unix(1, 0)
	for _, tc := range []struct {
		name       string
		mutate     func(*testing.T)
		credential dispatcher.MintedCredential
		scheme     string
	}{
		{name: "missing scheme", credential: dispatcher.MintedCredential{Value: "secret"}},
		{name: "unknown scheme", credential: dispatcher.MintedCredential{Value: "secret", Scheme: "digest"}},
		{name: "expired response", credential: dispatcher.MintedCredential{Value: "secret", ExpiresAt: &expired}, scheme: "bearer"},
		{name: "wrong owner", credential: dispatcher.MintedCredential{Value: "secret"}, scheme: "bearer", mutate: func(t *testing.T) { t.Setenv(dispatcher.InputEnvVar(executor.InputPROwner), "foreign") }},
		{name: "wrong repo", credential: dispatcher.MintedCredential{Value: "secret"}, scheme: "bearer", mutate: func(t *testing.T) { t.Setenv(dispatcher.InputEnvVar(executor.InputPRRepo), "foreign") }},
		{name: "missing project", credential: dispatcher.MintedCredential{Value: "secret"}, scheme: "bearer", mutate: func(t *testing.T) { t.Setenv(executor.RepoProjectEnvVar, "") }},
		{name: "bad backlog stamp", credential: dispatcher.MintedCredential{Value: "secret"}, scheme: "bearer", mutate: func(t *testing.T) { t.Setenv(dispatcher.GaggleBacklogEnv, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupADOPodPoll(t, tc.scheme, func(int) dispatcher.MintedCredential { return tc.credential }, func(http.ResponseWriter, *http.Request) { t.Error("invalid configuration sent provider request") })
			if tc.mutate != nil {
				tc.mutate(t)
			}
			if result := runCIPollStage(context.Background(), io.Discard); result.Error == nil {
				t.Fatalf("accepted invalid configuration: %+v", result)
			}
		})
	}
}

func TestPodCIPollInputContract(t *testing.T) {
	for _, key := range []string{executor.InputCarryOutputs, executor.InputRetryFailedChecksMaxAttempts} {
		t.Run(key, func(t *testing.T) {
			fixture := defaultCIPollFixture()
			fixture.inputs[key] = "1"
			setPodCIPollEnv(t, fixture)
			if _, err := podCIPollConfig(fixture.capabilities); err == nil {
				t.Fatalf("silently ignored %s", key)
			}
		})
	}
	fixture := defaultCIPollFixture()
	fixture.inputs[executor.InputNoVerdict] = "continue"
	setPodCIPollEnv(t, fixture)
	cfg, err := podCIPollConfig(fixture.capabilities)
	if err != nil || !cfg.NoVerdictContinue {
		t.Fatalf("noVerdict dropped: %+v %v", cfg, err)
	}
}

func TestPodCIPollRefusesAmbientFallback(t *testing.T) {
	for _, tc := range []struct{ provider, want string }{
		{"", "explicit routed provider stamp"},
		{string(providers.ProviderADO), "explicit broker credential source"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			t.Setenv(executor.RepoProviderEnvVar, tc.provider)
			t.Setenv(executor.RepoOwnerEnvVar, "acme")
			t.Setenv(executor.RepoProjectEnvVar, "project")
			t.Setenv(executor.RepoNameEnvVar, "web")
			_, err := podCIPollPoller("ambient-token", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("missing pod authority: %v, want %q", err, tc.want)
			}
		})
	}
}
