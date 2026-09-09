package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
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
	t.Chdir(root)

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
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	identity, err := instance.NewLayout(root).EnsureIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(executor.InstanceIDEnvVar, identity)
	t.Setenv("GOOBERS_RUN_ID", "run-123456789")
	t.Setenv("GOOBERS_GAGGLE", "dogfood")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv(executor.TaskEnvVar, "publish-result")
	t.Setenv(executor.GooberEnvVar, "implementer")

	got, ok := stageAttribution(root)
	if !ok {
		t.Fatal("stageAttribution did not recognize complete run context")
	}
	if got.Instance != "MDB1" ||
		got.InstanceID != identity ||
		got.Gaggle != "dogfood" ||
		got.Workflow != "implementation" ||
		got.Task != "publish-result" ||
		got.Goober != "implementer" ||
		got.Run != "run-123456789" {
		t.Fatalf("attribution = %+v", got)
	}
}

func TestStageAttributionPreservesOriginatingInstance(t *testing.T) {
	root := t.TempDir()
	if _, err := instance.NewLayout(root).EnsureIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_RUN_ID", "run-origin")
	t.Setenv("GOOBERS_GAGGLE", "web")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv(executor.TaskEnvVar, "merge")
	t.Setenv(executor.InstanceIDEnvVar, "")
	if err := os.Unsetenv(executor.InstanceIDEnvVar); err != nil {
		t.Fatal(err)
	}
	if got, ok := stageAttribution(root); !ok || got.InstanceID != "" {
		t.Fatalf("missing pin gained worker identity: %+v, %v", got, ok)
	}
	for _, value := range []string{"0123456789abcdef0123456789abcdef", "", "invalid", strings.Repeat("0", 32)} {
		t.Run("identity-"+value, func(t *testing.T) {
			t.Setenv(executor.InstanceIDEnvVar, value)
			got, ok := stageAttribution(root)
			want := ""
			if value == "0123456789abcdef0123456789abcdef" {
				want = value
			}
			if !ok || got.InstanceID != want {
				t.Fatalf("attribution = %+v, %v; want originating identity %q, never worker root", got, ok, want)
			}
		})
	}
}

func TestStageAttributionIncludesCurrentRunCostReceipt(t *testing.T) {
	root := initDemo(t)
	runID := "run-123456789"
	set, report, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
	if err != nil || len(set.Gaggles) != 1 {
		t.Fatalf("load demo gaggle: %v (report: %+v)", err, report)
	}
	gaggle := set.Gaggles[0].Name
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	nanoAIU := int64(8_305_840_000)
	run, err := journal.Create(instance.NewLayout(root).ForGaggle(gaggle).RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          gaggle,
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		StartedAt:       now,
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventAgentLifecycle,
		Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "implementer",
			RunID: runID, Stage: "implement", Attempt: 1,
			Lifecycle: journal.AgentCompleted, StartedAt: now, UpdatedAt: now,
			Usage: journal.AgentUsage{Model: "gpt-5.6", NanoAIU: &nanoAIU},
		},
	}); err != nil {
		t.Fatalf("append agent usage: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run: %v", err)
	}

	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_GAGGLE", gaggle)
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv(executor.TaskEnvVar, "publish-result")
	t.Setenv(executor.GooberEnvVar, "implementer")

	got, ok := stageAttribution(root)
	if !ok || got.Cost == nil || got.Cost.NanoAIU == nil {
		t.Fatalf("stage attribution receipt = %+v, ok=%v", got.Cost, ok)
	}
	if *got.Cost.NanoAIU != nanoAIU || got.Cost.Model != "gpt-5.6" || got.Cost.JournalSequence == 0 {
		t.Fatalf("stage attribution receipt = %+v", got.Cost)
	}
	configPath := instance.NewLayout(root).ConfigFile()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\ncost:\n  enabled: false\n")...)
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	suppressed, ok := stageAttribution(root)
	if !ok || suppressed.Cost != nil || suppressed.Run != runID {
		t.Fatalf("disabled publication must retain attribution without cost: %+v", suppressed)
	}
	local := stageCostReceipt(root, runID)
	if local == nil || local.NanoAIU == nil || *local.NanoAIU != nanoAIU || local.JournalSequence != got.Cost.JournalSequence {
		t.Fatalf("disabling publication changed durable local accounting: %+v", local)
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
