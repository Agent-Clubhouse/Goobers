package main

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

func TestStageADOListReadPlanRequiresExplicitLaunchScope(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(layoutFor(root).SchedulerDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(executor.GaggleEnvVar, "own")
	// A stage pod: no readable instance config, the gaggle backlog settings
	// stamped by the dispatcher (#5957).
	t.Setenv(dispatcher.GaggleBacklogEnv, "{}")
	t.Setenv(executor.ConfigGenerationEnvVar, "pinned-run-generation")
	t.Setenv(providersnapshot.EnvVar, "evaluation")
	t.Setenv(executor.ProviderReadBindingEnvVar, "")
	t.Setenv(executor.ProviderReadGenerationEnvVar, "")
	calls := 0
	transport := scopedStageReadTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/wiql") || request.Header.Get("Authorization") == "" {
			t.Fatalf("unexpected or unauthenticated list request: %s %s", request.Method, request.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"workItems":[]}`)), Request: request}, nil
	})
	previous := newADOProviderForStage
	newADOProviderForStage = func(repo providers.RepositoryRef, source providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		p := providers.NewADOProvider(repo.Owner, repo.Project, "", providers.WithADOCredentialSource(source))
		p.Client = transport
		return p, nil
	}
	t.Cleanup(func() { newADOProviderForStage = previous })
	source, err := providers.NewADODeliveredCredentialSourceWithExpiry("pat", "same-token", "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
	read := func() {
		t.Helper()
		p, err := newRegisteredADOProviderForStage(stageProviderConfig{root: root, repo: repo, adoSource: source})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.(*providers.ADOProvider).ListWorkItems(t.Context(), providers.ListWorkItemsRequest{Repository: repo, State: "all"}); err != nil {
			t.Fatal(err)
		}
	}
	read()
	read()
	if calls != 2 {
		t.Fatal("gaggle/config alone inferred automation scope", calls)
	}
	t.Setenv(executor.ProviderReadBindingEnvVar, "automation")
	t.Setenv(executor.ProviderReadGenerationEnvVar, "policy-1")
	read()
	read()
	if calls != 3 {
		t.Fatal("explicit ADO stage scope failed to share", calls)
	}
	t.Setenv(executor.ProviderReadBindingEnvVar, "")
	t.Setenv(executor.ProviderReadGenerationEnvVar, "")
	read()
	read()
	if calls != 5 {
		t.Fatal("human/unknown stage reused automation snapshot", calls)
	}
}
