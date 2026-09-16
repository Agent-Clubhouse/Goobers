package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func selectedAttempt() (Config, Attempt) {
	cfg, attempt := testConfig(), testAttempt()
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "base", Name: "repo"}
	source := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fork", Name: "repo",
		Checkout: &apiv1.CheckoutSpec{Sparse: []string{"src"}}}
	cfg.WorkspaceRepositories = map[string][]apiv1.RepoRef{attempt.Gaggle: {source}}
	attempt.Workspace, attempt.WorkspaceRepository = string(apiv1.WorkspaceRepoReadOnly), base
	attempt.PartialClone = true
	attempt.Checkout = source.Checkout
	attempt.WorkspaceRevision = &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{Provider: source.Provider, Owner: source.Owner, Name: source.Name},
		CommitSHA:  strings.Repeat("a", 40), SourceRef: "refs/heads/same-name",
	}
	return cfg, attempt
}

func TestWorkspaceRevisionPodTransport(t *testing.T) {
	for _, template := range []bool{false, true} {
		t.Run(map[bool]string{false: "image", true: "template"}[template], func(t *testing.T) {
			cfg, attempt := selectedAttempt()
			var pod *corev1.Pod
			var err error
			if template {
				d := testDeployment()
				d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
					{Name: EnvWorkspaceBranch, Value: "wrong"}, {Name: EnvWorkspaceDelta, Value: "wrong"},
					{Name: EnvWorkspaceCheckout, Value: "wrong"}, {Name: EnvStageSyncBase, Value: "true"},
					{Name: EnvCheckoutCapability, Value: "repo:push"},
				}
				pod, err = RenderFromTemplate(cfg, attempt, linuxRunner(), d)
			} else {
				pod, err = RenderPod(cfg, attempt, linuxRunner())
			}
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, e := range pod.Spec.Containers[0].Env {
				env[e.Name] = expandPodEnv(e.Value, env)
			}
			var got apiv1.WorkspaceRevision
			var checkout WorkspaceCheckout
			if err := json.Unmarshal([]byte(env[EnvWorkspaceRevision]), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(&got, attempt.WorkspaceRevision) {
				t.Fatalf("selected identity changed: %+v", got)
			}
			if err := json.Unmarshal([]byte(env[EnvWorkspaceCheckout]), &checkout); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(checkout.Repository, cfg.WorkspaceRepositories[attempt.Gaggle][0]) || !checkout.PartialClone {
				t.Fatalf("configured source/policy lost: %+v", checkout)
			}
			for _, key := range []string{EnvWorkspaceBranch, EnvWorkspaceDelta, EnvStageSyncBase, EnvCheckoutCapability} {
				if env[key] != "" {
					t.Fatalf("writable control %s=%q", key, env[key])
				}
			}
		})
	}
}

func TestWorkspaceRevisionDispatchRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   string
		mutate func(*Config, *Attempt)
	}{
		{"missing fork", workspacerevision.CodeUnauthorized, func(c *Config, _ *Attempt) { c.WorkspaceRepositories = nil }},
		{"other gaggle", workspacerevision.CodeUnauthorized, func(_ *Config, a *Attempt) { a.Gaggle = "other" }},
		{"wrong service", workspacerevision.CodeUnauthorized, func(_ *Config, a *Attempt) { a.WorkspaceRevision.Repository.URL = "https://other.example/fork/repo" }},
		{"abbreviated sha", workspacerevision.CodeInvalid, func(_ *Config, a *Attempt) { a.WorkspaceRevision.CommitSHA = "abc123" }},
		{"writable", workspacerevision.CodeInvalid, func(_ *Config, a *Attempt) { a.Workspace = "repo" }},
		{"branch", workspacerevision.CodeConflict, func(_ *Config, a *Attempt) { a.WorkspaceBranch = "same-name" }},
		{"delta", workspacerevision.CodeConflict, func(_ *Config, a *Attempt) { a.WorkspaceDelta = "digest" }},
		{"sync", workspacerevision.CodeConflict, func(_ *Config, a *Attempt) { a.SyncBase = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, attempt := selectedAttempt()
			tc.mutate(&cfg, &attempt)
			_, err := RenderPod(cfg, attempt, linuxRunner())
			var refusal *workspacerevision.Error
			if !errors.As(err, &refusal) || refusal.Code != tc.code {
				t.Fatalf("error=%v, want %s", err, tc.code)
			}
		})
	}
	for _, key := range []string{EnvWorkspaceRevision, EnvWorkspaceCheckout} {
		cfg, attempt := selectedAttempt()
		attempt.Env = map[string]string{key: "override"}
		if _, err := RenderPod(cfg, attempt, linuxRunner()); err == nil {
			t.Fatalf("%s is not reserved", key)
		}
	}
}

func TestWorkspaceRevisionCredentialRequest(t *testing.T) {
	_, attempt := selectedAttempt()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			RunID             string                   `json:"runId"`
			Stage             string                   `json:"stage"`
			Capabilities      []string                 `json:"capabilities"`
			WorkspaceRevision *apiv1.WorkspaceRevision `json:"workspaceRevision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(request.WorkspaceRevision, attempt.WorkspaceRevision) ||
			len(request.Capabilities) != 0 || request.RunID != "run" || request.Stage != "inspect" {
			t.Errorf("unexpected credential request: %+v", request)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"credentials": []MintedCredential{
			{Capability: WorkspaceRevisionCheckoutCapability, Value: "source-only"},
		}})
	}))
	defer server.Close()
	client := CredentialResolveClient{BaseURL: server.URL}
	creds, err := client.ResolveCheckout(context.Background(), "run", "inspect", attempt.WorkspaceRevision)
	if err != nil || len(creds) != 1 || creds[0].Capability != WorkspaceRevisionCheckoutCapability {
		t.Fatalf("checkout resolve: %+v %v", creds, err)
	}
}

func TestWorkspaceRevisionAnonymousCredentialResponse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		anonymous bool
	}{
		{"configured public", http.StatusOK, `{"credentials":[]}`, true},
		{"missing authorization", http.StatusOK, `{}`, false},
		{"null authorization", http.StatusOK, `{"credentials":null}`, false},
		{"configured credential failure", http.StatusForbidden, `{"code":"workspace_revision_unauthorized"}`, false},
		{"empty credential value", http.StatusOK, `{"credentials":[{"capability":"workspace-revision:checkout","value":""}]}`, false},
		{"unrelated credential", http.StatusOK, `{"credentials":[{"capability":"repo:push","value":"base-token"}]}`, false},
		{"duplicate checkout", http.StatusOK, `{"credentials":[{"capability":"workspace-revision:checkout","value":"one"},{"capability":"workspace-revision:checkout","value":"two"}]}`, false},
		{"checkout plus unrelated", http.StatusOK, `{"credentials":[{"capability":"workspace-revision:checkout","value":"one"},{"capability":"repo:push","value":"base-token"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, attempt := selectedAttempt()
			client := CredentialResolveClient{BaseURL: server.URL}
			creds, err := client.ResolveCheckout(context.Background(), "run", "stage", attempt.WorkspaceRevision)
			if tc.anonymous {
				if err != nil || len(creds) != 1 || !creds[0].Anonymous || creds[0].Capability != WorkspaceRevisionCheckoutCapability || creds[0].Value != "" {
					t.Fatalf("anonymous grant=%+v, %v", creds, err)
				}
			} else if err == nil || len(creds) != 0 {
				t.Fatalf("failed credential resolution became anonymous: %+v %v", creds, err)
			}
		})
	}
}

func TestWorkspaceRevisionCredentialFailureTaxonomy(t *testing.T) {
	for _, code := range []string{
		workspacerevision.CodeInvalid, workspacerevision.CodeUnauthorized,
		workspacerevision.CodeConflict, workspacerevision.CodeAcquisition,
		workspacerevision.CodeObjectType, workspacerevision.CodeSHAMismatch,
	} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": "refused"})
			}))
			defer server.Close()
			_, attempt := selectedAttempt()
			client := CredentialResolveClient{BaseURL: server.URL}
			_, err := client.ResolveCheckout(context.Background(), "run", "stage", attempt.WorkspaceRevision)
			var refusal *workspacerevision.Error
			if !errors.As(err, &refusal) || refusal.Code != code || refusal.NonRetryable() != (code != workspacerevision.CodeAcquisition) {
				t.Fatalf("error=%v, want %s", err, code)
			}
		})
	}
}
