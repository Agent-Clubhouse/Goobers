package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func item(id, title, body string) providers.WorkItem {
	return providers.WorkItem{ID: id, Title: title, Body: body}
}

func TestReorderContestedBacklogItemsWarnsAndRetainsFIFOOnProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	provider := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) {
		p.BaseURL = server.URL
	})
	var stderr bytes.Buffer
	env := backlogQueryEnv{
		repo:   providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"},
		stderr: &stderr,
	}
	eligible := []providers.WorkItem{
		item("1", "first", "a.go"),
		item("2", "second", "b.go"),
	}

	got := reorderContestedBacklogItems(t.Context(), env, provider, eligible, len(eligible))
	if !reflect.DeepEqual(got, eligible) {
		t.Fatalf("order = %+v, want unchanged %+v", got, eligible)
	}
	wantWarning := fmt.Sprintf(
		"warning: contested-file dispatch awareness unavailable (GET %s/repos/acme/widgets/pulls?per_page=100&state=open failed: status 503: provider unavailable); using FIFO order\n",
		server.URL,
	)
	if stderr.String() != wantWarning {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantWarning)
	}
}

func TestReorderContestedBacklogItemsKeepsResweepAfterForwardPartition(t *testing.T) {
	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addOpenPR(10, providers.DefaultBranchNamespace+"implementation/run-10", "main", "head10", "base",
		false, nil, []fakePRFile{{path: "hot.go"}})
	provider := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) {
		p.BaseURL = server.server.URL
	})
	var stderr bytes.Buffer
	env := backlogQueryEnv{
		repo:   providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"},
		stderr: &stderr,
	}
	eligible := []providers.WorkItem{
		item("1", "contested forward", "hot.go"),
		item("2", "clean forward", "clean.go"),
		item("3", "read-only resweep", "clean.go"),
	}
	t.Setenv(executor.InputEnvVar("contestedFileMinPRs"), "1")

	got := reorderContestedBacklogItems(t.Context(), env, provider, eligible, 2)
	gotIDs := []string{got[0].ID, got[1].ID, got[2].ID}
	if want := []string{"2", "1", "3"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("order = %v, want %v", gotIDs, want)
	}
	wantWarning := "contested-file dispatch: deprioritized 1 contested issue(s) [1] behind 1 disjoint one(s)\n"
	if stderr.String() != wantWarning {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantWarning)
	}
}

func TestReorderContestedBacklogItemsUsesConfiguredBranchNamespace(t *testing.T) {
	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addOpenPR(10, "acme/implementation/run-10", "main", "head10", "base",
		false, nil, []fakePRFile{{path: "hot.go"}})
	server.addOpenPR(11, providers.DefaultBranchNamespace+"implementation/run-11", "main", "head11", "base",
		false, nil, []fakePRFile{{path: "other.go"}})
	provider := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) {
		p.BaseURL = server.server.URL
	})
	var stderr bytes.Buffer
	env := backlogQueryEnv{
		repo:   providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"},
		stderr: &stderr,
	}
	eligible := []providers.WorkItem{
		item("1", "contested", "hot.go"),
		item("2", "clean", "other.go"),
	}
	t.Setenv(executor.BranchNamespaceEnvVar, "acme")
	t.Setenv(executor.InputEnvVar("contestedFileMinPRs"), "1")

	got := reorderContestedBacklogItems(t.Context(), env, provider, eligible, len(eligible))
	if gotIDs := []string{got[0].ID, got[1].ID}; !reflect.DeepEqual(gotIDs, []string{"2", "1"}) {
		t.Fatalf("order = %v, want [2 1]", gotIDs)
	}
}

func TestGatherImplementContextProviderFailureWritesTypedFailureWithoutPartialEvidence(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(10, providers.DefaultBranchNamespace+"implementation/run-10", "main", "head10", "base",
		false, nil, []fakePRFile{{path: "first.go"}})
	server.addOpenPR(11, providers.DefaultBranchNamespace+"implementation/run-11", "main", "head11", "base",
		false, nil, []fakePRFile{{path: "second.go"}})
	server.setPullRequestFilesFailure(11, http.StatusServiceUnavailable, "provider unavailable")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-implementation-context-failure")

	workDir := t.TempDir()
	resultFile := filepath.Join(workDir, "implementation-context-result.json")
	t.Setenv(executor.InputEnvVar("resultFile"), resultFile)
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "gather-implement-context", root)
	if code != 1 || stdout != "" {
		t.Fatalf("result = code %d, stdout %q, want code 1 and empty stdout", code, stdout)
	}
	errorText := fmt.Sprintf(
		"GET %s/repos/your-org/your-repo/pulls/11/files?per_page=100 failed: status 503: provider unavailable",
		server.server.URL,
	)
	wantStderr := "error: gather implementation hot-file map: " + errorText + "\n"
	if stderr != wantStderr {
		t.Fatalf("stderr = %q, want %q", stderr, wantStderr)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read typed failure: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode typed failure: %v", err)
	}
	want := map[string]any{
		"errorCode":      errorCodeServerError,
		"errorMessage":   "gather implementation hot-file map: " + errorText,
		"errorRetryable": true,
		"integrity":      "unapproved",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("typed failure = %#v, want %#v", got, want)
	}
	if strings.Contains(string(data), "hotFileMap") || strings.Contains(string(data), "first.go") {
		t.Fatalf("typed failure contains partial hot-file evidence: %s", data)
	}
	filesRequests, checkRequests := server.requestCounts()
	if filesRequests != 1 || checkRequests != 0 {
		t.Fatalf("provider requests = files:%d checks:%d, want files:1 checks:0", filesRequests, checkRequests)
	}
}
