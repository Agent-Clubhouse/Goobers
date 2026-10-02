package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// Native Azure Boards linking in open-pr is best-effort (#5925): a stage that
// was not delivered ado:work-items:write still opens its pull request, with
// the text reference and a note, while a delivered credential that Azure
// DevOps rejects still fails the stage.

// adoWorkItemLinkServer is a fake Azure DevOps API for one open-pr stage: it
// opens pull request 7, serves work item 42 and records every work-item PATCH.
type adoWorkItemLinkServer struct {
	mu            sync.Mutex
	description   string
	posts         int
	workItemPatch int
	rejectPatch   bool
}

func (s *adoWorkItemLinkServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}})
		case http.MethodPost:
			var posted struct {
				Description string `json:"description"`
			}
			_ = json.NewDecoder(r.Body).Decode(&posted)
			s.description = posted.Description
			s.posts++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pullRequestId": 7,
				"url":           "api-pr-url",
				"_links":        map[string]any{"web": map[string]string{"href": "https://ado.example/pr/7"}},
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/7", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"pullRequestId": 7,
			"repository": map[string]any{
				"id":      "repo-guid",
				"project": map[string]string{"id": "project-guid", "name": "project"},
			},
		})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "rev": 3, "relations": []any{}})
		case http.MethodPatch:
			s.workItemPatch++
			if s.rejectPatch {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "access denied"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "rev": 4})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return mux
}

// runADOOpenPRStageWithClaimedItem runs `goobers open-pr` through the real
// ShellExecutor for a run that claimed work item 42, delivering only the
// credentials of the capabilities declared.
func runADOOpenPRStageWithClaimedItem(t *testing.T, server *adoWorkItemLinkServer, declared ...capability.Capability) apiv1.ResultEnvelope {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("helper process wrapper uses a POSIX shell")
	}
	root := initDemo(t)
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Repos = []instance.RepoRef{{
		Provider: "ado", Owner: "org", Project: "project", Name: "repo",
		Token: instance.TokenRef{Env: "ADO_OPEN_PR_PAT"},
	}}
	if err := instance.WriteConfig(layoutFor(root).ConfigFile(), cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	recordClaimedItemForRun(t, root, "run-ado", "42", "Claimed work item")

	httpServer := httptest.NewServer(server.handler())
	t.Cleanup(httpServer.Close)

	t.Setenv("ADO_OPEN_PR_PAT", "test-pat")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "ado-repo", Env: "ADO_OPEN_PR_PAT"}})
	if err != nil {
		t.Fatal(err)
	}
	grants := make([]credentials.Grant, 0, len(declared))
	names := make([]string, 0, len(declared))
	for _, c := range declared {
		grants = append(grants, credentials.Grant{Capability: string(c), Ref: "ado-repo"})
		names = append(names, string(c))
	}
	registry, _ := journal.DefaultScrubber()
	injector, err := credentials.NewInjector(resolver, grants, registry)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := executor.NewShellExecutor(injector, respondToFindingsTestRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "goobers")
	script := "#!/bin/sh\nexec \"$GOOBERS_TEST_BINARY\" -test.run=^TestOpenPRADOStageHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	shell.InstanceRoot = root
	shell.SelfBin = wrapper
	result, err := shell.Run(context.Background(), apiv1.InvocationEnvelope{
		TaskID:       "open-pr",
		WorkflowID:   "implementation",
		RunID:        "run-ado",
		Gaggle:       "goobers",
		Workspace:    t.TempDir(),
		Capabilities: names,
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "project", Name: "repo"},
	}, apiv1.DeterministicRun{
		Command: []string{"goobers", "open-pr", root},
		Env: map[string]string{
			"GOOBERS_TEST_ADO_OPEN_PR_HELPER": "1",
			"GOOBERS_TEST_ADO_API_URL":        httpServer.URL,
			"GOOBERS_TEST_BINARY":             testBinary,
		},
	})
	if err != nil {
		t.Fatalf("execute open-pr stage: %v", err)
	}
	return result
}

func recordClaimedItemForRun(t *testing.T, root, runID, id, title string) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "query-backlog", Status: "success",
		Outputs: map[string]any{"id": id, "title": title},
	}); err != nil {
		t.Fatalf("record claimed item: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

// TestOpenPRADOClaimedItemOpensWithoutWorkItemCapability is the shipped
// implementation workflow's shape: open-pr declares only provider:pr:write,
// and the run claimed an ADO work item. The pull request opens, its body says
// it is not linked natively, and no work item is written.
func TestOpenPRADOClaimedItemOpensWithoutWorkItemCapability(t *testing.T) {
	server := &adoWorkItemLinkServer{}
	result := runADOOpenPRStageWithClaimedItem(t, server, capability.ProviderPRWrite)
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("open-pr stage status = %q, want success: %+v", result.Status, result)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.posts != 1 {
		t.Fatalf("pull request POSTs = %d, want 1", server.posts)
	}
	if server.workItemPatch != 0 {
		t.Fatalf("work-item PATCHes = %d, want none without %s", server.workItemPatch, capability.ADOWorkItemsWrite)
	}
	if !strings.Contains(server.description, "Not linked natively to work item #42") ||
		!strings.Contains(server.description, string(capability.ADOWorkItemsWrite)) {
		t.Fatalf("pull request description = %q, want the text-only link note", server.description)
	}
}

// TestOpenPRADOClaimedItemLinksNativelyWhenDeclared: with ado:work-items:write
// declared and delivered, open-pr writes the native link and adds no note.
func TestOpenPRADOClaimedItemLinksNativelyWhenDeclared(t *testing.T) {
	server := &adoWorkItemLinkServer{}
	result := runADOOpenPRStageWithClaimedItem(t, server, capability.ProviderPRWrite, capability.ADOWorkItemsWrite)
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("open-pr stage status = %q, want success: %+v", result.Status, result)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.workItemPatch != 1 {
		t.Fatalf("work-item PATCHes = %d, want one native link", server.workItemPatch)
	}
	if strings.Contains(server.description, "Not linked natively") {
		t.Fatalf("pull request description = %q, want no text-only note when linking natively", server.description)
	}
}

// TestOpenPRADOClaimedItemFailsWhenDeliveredCredentialIsRejected: a declared
// credential that Azure DevOps rejects still fails the stage (fail closed).
func TestOpenPRADOClaimedItemFailsWhenDeliveredCredentialIsRejected(t *testing.T) {
	server := &adoWorkItemLinkServer{rejectPatch: true}
	result := runADOOpenPRStageWithClaimedItem(t, server, capability.ProviderPRWrite, capability.ADOWorkItemsWrite)
	if result.Status == apiv1.ResultSuccess {
		t.Fatalf("open-pr stage status = %q, want failure when ADO rejects the work-item link", result.Status)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.workItemPatch == 0 {
		t.Fatal("open-pr did not attempt the native link with the delivered credential")
	}
}

type fakeADOWorkItemLinker struct{ err error }

func (f fakeADOWorkItemLinker) LinkPullRequestToWorkItem(context.Context, providers.RepositoryRef, providers.RepositoryRef, string, string) error {
	return f.err
}

type recordingOpenPRProvider struct{ body string }

func (p *recordingOpenPRProvider) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}

func (p *recordingOpenPRProvider) OpenPullRequest(_ context.Context, req providers.PullRequestRequest) (providers.PullRequestResult, error) {
	p.body = req.Body
	return providers.PullRequestResult{ID: "7", Number: 7}, nil
}

func TestOpenPullRequestWithADOLinkNotesTextOnlyLink(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
	root := t.TempDir()
	cases := []struct {
		name     string
		linker   adoPullRequestWorkItemLinker
		wantCode int
		wantNote bool
	}{
		{name: "text only", linker: textOnlyADOWorkItemLink{}, wantCode: 0, wantNote: true},
		{name: "native", linker: fakeADOWorkItemLinker{}, wantCode: 0},
		{name: "native rejected", linker: fakeADOWorkItemLinker{err: errors.New("access denied")}, wantCode: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			provider := &recordingOpenPRProvider{}
			var stderr strings.Builder
			_, code := openPullRequestWithADOLink(context.Background(), provider, tc.linker, repo, root, "42", true,
				providers.PullRequestRequest{Repository: repo, Body: "Fixes #42"}, nil, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tc.wantCode, stderr.String())
			}
			if got := strings.Contains(provider.body, "Not linked natively to work item #42"); got != tc.wantNote {
				t.Fatalf("body = %q, want note %t", provider.body, tc.wantNote)
			}
			if !strings.HasPrefix(provider.body, "Fixes #42") {
				t.Fatalf("body = %q, want the text reference kept", provider.body)
			}
		})
	}
}

type reviewRecordingProvider struct {
	got providers.ReviewRequest
	err error
}

func (p *reviewRecordingProvider) RequestReview(_ context.Context, req providers.ReviewRequest) error {
	p.got = req
	return p.err
}

func TestParseReviewers(t *testing.T) {
	got := parseReviewers(" @Alice, bob\nalice,,carol ")
	want := []string{"Alice", "bob", "carol"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parseReviewers = %v, want %v", got, want)
	}
	if parseReviewers("") != nil {
		t.Fatal("empty input must yield no reviewers")
	}
}

func TestRequestOpenPRReviewers(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "o", Name: "r"}
	pr := providers.PullRequestResult{ID: "7", Number: 7}
	var stderr strings.Builder

	if got := requestOpenPRReviewers(context.Background(), &reviewRecordingProvider{}, repo, pr, nil, &stderr); got != nil {
		t.Fatalf("no reviewers must be a no-op, got %v", got)
	}

	ok := &reviewRecordingProvider{}
	got := requestOpenPRReviewers(context.Background(), ok, repo, pr, []string{"a", "b"}, &stderr)
	if got["reviewersRequested"] != "a,b" || ok.got.PullID != "7" || len(ok.got.Reviewers) != 2 {
		t.Fatalf("success: extras=%v req=%+v", got, ok.got)
	}

	fail := &reviewRecordingProvider{err: errors.New("author cannot review")}
	got = requestOpenPRReviewers(context.Background(), fail, repo, pr, []string{"a"}, &stderr)
	if got["reviewersRequestError"] != "author cannot review" || got["reviewersRequested"] != "" {
		t.Fatalf("failure must be recorded not raised: %v", got)
	}
	if !strings.Contains(stderr.String(), "warning: could not request review") {
		t.Fatalf("missing warning: %q", stderr.String())
	}

	got = requestOpenPRReviewers(context.Background(), struct{}{}, repo, pr, []string{"a"}, &stderr)
	if got["reviewersRequestError"] == "" {
		t.Fatalf("unsupported provider must be recorded: %v", got)
	}
}

func TestWriteOpenPRResultIncludesReviewerExtras(t *testing.T) {
	f := filepath.Join(t.TempDir(), "r.json")
	if err := writeOpenPRResult(f, true, 7, "u", map[string]string{"reviewersRequested": "a"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f)
	if !strings.Contains(string(data), `"reviewersRequested":"a"`) || !strings.Contains(string(data), `"opened":"true"`) {
		t.Fatalf("result = %s", data)
	}
}
