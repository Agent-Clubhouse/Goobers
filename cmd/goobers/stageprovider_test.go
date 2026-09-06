package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestNewProviderForStageDispatchesGitHub(t *testing.T) {
	root := initDemo(t)
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesRead)), "read-token")

	provider, err := newProviderForStage(root, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}, true)
	if err != nil {
		t.Fatalf("newProviderForStage: %v", err)
	}
	if provider.Kind() != providers.ProviderGitHub {
		t.Fatalf("provider kind = %q, want %q", provider.Kind(), providers.ProviderGitHub)
	}
}

func TestNewMergeReviewProviderDispatchesADO(t *testing.T) {
	previous := stageProviderFactories[providers.ProviderADO]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = previous })
	stageProviderFactories[providers.ProviderADO] = func(cfg stageProviderConfig) (providers.Provider, error) {
		return providers.NewADOProvider(cfg.repo.Owner, cfg.repo.Project, "ado-token"), nil
	}

	provider, err := newMergeReviewProvider(t.TempDir(), providers.RepositoryRef{
		Provider: providers.ProviderADO,
		Owner:    "contoso",
		Project:  "project",
		Name:     "repo",
	}, false)
	if err != nil {
		t.Fatalf("newMergeReviewProvider: %v", err)
	}
	if provider.Kind() != providers.ProviderADO {
		t.Fatalf("provider kind = %q, want %q", provider.Kind(), providers.ProviderADO)
	}
}

func TestNewMergeReviewProviderAsDispatchesADOOperationProvider(t *testing.T) {
	previous := stageProviderFactories[providers.ProviderADO]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = previous })
	stageProviderFactories[providers.ProviderADO] = func(cfg stageProviderConfig) (providers.Provider, error) {
		return providers.NewADOProvider(cfg.repo.Owner, cfg.repo.Project, "ado-token"), nil
	}

	provider, err := newMergeReviewProviderAs[*providers.ADOProvider](
		t.TempDir(),
		providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "contoso", Project: "project", Name: "repo"},
		false,
	)
	if err != nil {
		t.Fatalf("newMergeReviewProviderAs: %v", err)
	}
	if provider.Kind() != providers.ProviderADO {
		t.Fatalf("provider kind = %q, want %q", provider.Kind(), providers.ProviderADO)
	}
}

func TestStageProviderRegistryIncludesBuiltInProviders(t *testing.T) {
	for _, kind := range []providers.ProviderKind{
		providers.ProviderGitHub,
		providers.ProviderADO,
		providers.ProviderGitea,
	} {
		if stageProviderFactories[kind] == nil {
			t.Errorf("provider %q is not registered", kind)
		}
	}
}

func TestNewProviderForStageWiresADOMutationRecorder(t *testing.T) {
	root := initDemo(t)
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldwd) }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/org/project/_apis/git/repositories/repo/pullrequests/42/threads" {
			t.Fatalf("request = %s %s, want POST /org/project/_apis/git/repositories/repo/pullrequests/42/threads", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"comments":[{"id":1,"content":"ok","commentType":"text","author":{"displayName":"Goobers Bot","uniqueName":"bot@example.com","id":"author-guid"},"publishedDate":"2026-08-08T10:00:00Z"}]}`))
	}))
	defer server.Close()

	previous := stageProviderFactories[providers.ProviderADO]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = previous })
	stageProviderFactories[providers.ProviderADO] = func(cfg stageProviderConfig) (providers.Provider, error) {
		return providers.NewADOProvider(cfg.repo.Owner, cfg.repo.Project, "token", func(p *providers.ADOProvider) {
			p.BaseURL = server.URL
		}), nil
	}

	provider, err := newProviderForStage(root, providers.RepositoryRef{
		Provider: providers.ProviderADO,
		Owner:    "org",
		Project:  "project",
		Name:     "repo",
	}, false, withStageProviderMutations("pr"))
	if err != nil {
		t.Fatalf("newProviderForStage: %v", err)
	}
	if _, err := provider.(*providers.ADOProvider).PostPullRequestThreadComment(context.Background(), providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}, "42", "ok"); err != nil {
		t.Fatalf("PostPullRequestThreadComment: %v", err)
	}
	data, err := os.ReadFile(mutationsSidecarFile)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if !strings.Contains(string(data), `"provider":"ado"`) || !strings.Contains(string(data), `"kind":"pr"`) || !strings.Contains(string(data), `"id":"42"`) {
		t.Fatalf("sidecar = %s, want provider=ado kind=pr id=42", string(data))
	}
}

func TestStageAttributionUsesInjectedRunContext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "MDB1")
	t.Setenv("GOOBERS_RUN_ID", "run-123456789")
	t.Setenv("GOOBERS_GAGGLE", "efunhouse")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv(executor.TaskEnvVar, "publish-result")
	t.Setenv(executor.GooberEnvVar, "implementer")

	got, ok := stageAttribution(root)
	if !ok {
		t.Fatal("stageAttribution did not recognize complete run context")
	}
	if got.Instance != "MDB1" ||
		got.Gaggle != "efunhouse" ||
		got.Workflow != "implementation" ||
		got.Task != "publish-result" ||
		got.Goober != "implementer" ||
		got.Run != "run-123456789" {
		t.Fatalf("attribution = %+v", got)
	}
}

func TestStageAttributionRequiresCompleteStageContext(t *testing.T) {
	t.Setenv("GOOBERS_RUN_ID", "run-1")
	t.Setenv("GOOBERS_GAGGLE", "gaggle")
	t.Setenv("GOOBERS_WORKFLOW", "workflow")
	t.Setenv(executor.TaskEnvVar, "")

	if got, ok := stageAttribution(t.TempDir()); ok {
		t.Fatalf("stageAttribution = %+v, want incomplete standalone context ignored", got)
	}
}

func TestNewProviderForStageRejectsUnregisteredProvider(t *testing.T) {
	_, err := newProviderForStage(t.TempDir(), providers.RepositoryRef{Provider: "unknown"}, true)
	if err == nil || !strings.Contains(err.Error(), `provider "unknown" is not registered`) {
		t.Fatalf("error = %v, want unregistered-provider error", err)
	}
}

func TestNewProviderForStageUsesRequestedCapability(t *testing.T) {
	const token = "pr-token"
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), token)

	previous := newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = previous })
	var gotToken string
	newGitHubProvider = func(token string, _ ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		gotToken = token
		return providers.NewGitHubProvider(token)
	}

	_, err := newProviderForStage(
		t.TempDir(),
		providers.RepositoryRef{Provider: providers.ProviderGitHub},
		false,
		withStageProviderCapability(capability.ProviderPRWrite),
	)
	if err != nil {
		t.Fatalf("newProviderForStage: %v", err)
	}
	if gotToken != token {
		t.Fatalf("token = %q, want %q", gotToken, token)
	}
}

func TestNewProviderForStageObservesResolvedToken(t *testing.T) {
	const token = "branch-token"
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubBranchDelete)), token)

	var observed string
	_, err := newProviderForStage(
		t.TempDir(),
		providers.RepositoryRef{Provider: providers.ProviderGitHub},
		false,
		withStageProviderCapability(capability.GitHubBranchDelete),
		withStageProviderTokenObserver(func(token string) { observed = token }),
	)
	if err != nil {
		t.Fatalf("newProviderForStage: %v", err)
	}
	if observed != token {
		t.Fatalf("observed token = %q, want %q", observed, token)
	}
}

func TestNewProviderForStageAsRejectsUnsupportedConcreteOperation(t *testing.T) {
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesRead)), "read-token")

	_, err := newProviderForStageAs[*providers.ADOProvider](
		t.TempDir(),
		providers.RepositoryRef{Provider: providers.ProviderGitHub},
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "does not support this stage operation") {
		t.Fatalf("error = %v, want unsupported-operation error", err)
	}
}
