package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// staticTerminalTokenSource is the TokenSource seam the credential injector
// hands the terminal preparer, reduced to a fixed secret.
type staticTerminalTokenSource string

func (s staticTerminalTokenSource) Token(context.Context) (string, error) { return string(s), nil }

func giteaTerminalConfig(baseURL string) *instance.Config {
	return &instance.Config{Repos: []instance.RepoRef{{
		Provider: "gitea",
		Owner:    "acme",
		Name:     "widgets",
		BaseURL:  baseURL,
	}}}
}

// TestTerminalRepositoryRefCarriesConfiguredProvider pins the repo kind the
// terminal preparer acts on to the repo's OWN declared provider. This is not
// only dispatch input: it is stamped into the journal's ExternalRef.Provider
// for terminal branch-cleanup facts, which used to read "github" on a Gitea
// instance.
func TestTerminalRepositoryRefCarriesConfiguredProvider(t *testing.T) {
	tests := []struct {
		name string
		cfg  *instance.Config
		want providers.ProviderKind
	}{
		{
			name: "gitea repo keeps gitea",
			cfg:  giteaTerminalConfig("https://gitea.example.test"),
			want: providers.ProviderGitea,
		},
		{
			name: "github repo stays github",
			cfg:  &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "app"}}},
			want: providers.ProviderGitHub,
		},
		{
			name: "unset provider defaults to github for back-compat",
			cfg:  &instance.Config{Repos: []instance.RepoRef{{Owner: "acme", Name: "app"}}},
			want: providers.ProviderGitHub,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalRepositoryRefForProject(tc.cfg, apiv1.RepoRef{}).Provider; got != tc.want {
				t.Fatalf("provider = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTerminalRunAbortLabelProviderRoutesByRepoKind is the discriminating
// provider invariant: on a Gitea repo the run-abort labeler must not construct
// the GitHub provider. The GitHub seam is a tripwire for incorrect dispatch.
func TestTerminalRunAbortLabelProviderRoutesByRepoKind(t *testing.T) {
	previousGitHub := newRunAbortLabelProvider
	newRunAbortLabelProvider = func(providers.TokenSource) workItemUpdater {
		t.Error("gitea-routed run-abort label constructed the GitHub provider")
		return fakeWorkItemUpdater(func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
			return providers.WorkItem{}, nil
		})
	}
	t.Cleanup(func() { newRunAbortLabelProvider = previousGitHub })

	previousGitea := newGiteaRunAbortLabelProvider
	var gotBaseURL string
	var gotToken string
	newGiteaRunAbortLabelProvider = func(baseURL string, source providers.TokenSource) workItemUpdater {
		gotBaseURL = baseURL
		token, err := source.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		gotToken = token
		return fakeWorkItemUpdater(func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
			return providers.WorkItem{}, nil
		})
	}
	t.Cleanup(func() { newGiteaRunAbortLabelProvider = previousGitea })

	cfg := giteaTerminalConfig("https://gitea.example.test")
	provider, err := newTerminalRunAbortLabelProviderForProject(cfg, apiv1.RepoRef{}, staticTerminalTokenSource("gitea-pat"))
	if err != nil {
		t.Fatal(err)
	}
	if provider == nil {
		t.Fatal("nil provider")
	}
	if gotBaseURL != "https://gitea.example.test" {
		t.Fatalf("baseURL = %q, want the configured gitea root", gotBaseURL)
	}
	if gotToken != "gitea-pat" {
		t.Fatalf("token = %q, want the gitea capability token", gotToken)
	}
}

// TestTerminalRunAbortLabelProviderKeepsGitHubArm is the retained GitHub
// coverage: a GitHub repo must still build the GitHub provider and must never
// touch the Gitea arm.
func TestTerminalRunAbortLabelProviderKeepsGitHubArm(t *testing.T) {
	previousGitea := newGiteaRunAbortLabelProvider
	newGiteaRunAbortLabelProvider = func(string, providers.TokenSource) workItemUpdater {
		t.Error("github-routed run-abort label constructed the Gitea provider")
		return nil
	}
	t.Cleanup(func() { newGiteaRunAbortLabelProvider = previousGitea })

	previousGitHub := newRunAbortLabelProvider
	var constructed bool
	var gotToken string
	newRunAbortLabelProvider = func(source providers.TokenSource) workItemUpdater {
		constructed = true
		token, err := source.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		gotToken = token
		return fakeWorkItemUpdater(func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
			return providers.WorkItem{}, nil
		})
	}
	t.Cleanup(func() { newRunAbortLabelProvider = previousGitHub })

	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "app"}}}
	if _, err := newTerminalRunAbortLabelProviderForProject(cfg, apiv1.RepoRef{}, staticTerminalTokenSource("ghp-token")); err != nil {
		t.Fatal(err)
	}
	if !constructed {
		t.Fatal("github arm did not construct the GitHub provider")
	}
	if gotToken != "ghp-token" {
		t.Fatalf("token = %q", gotToken)
	}
}

func TestTerminalRunAbortLabelProviderRejectsUnsupportedKind(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "ado", Owner: "acme", Name: "app"}}}
	if _, err := newTerminalRunAbortLabelProviderForProject(cfg, apiv1.RepoRef{}, staticTerminalTokenSource("t")); err == nil {
		t.Fatal("expected an unsupported-provider error rather than a GitHub fallthrough")
	}
}

func TestTerminalRunAbortLabelProviderRequiresGiteaBaseURL(t *testing.T) {
	cfg := giteaTerminalConfig("")
	if _, err := newTerminalRunAbortLabelProviderForProject(cfg, apiv1.RepoRef{}, staticTerminalTokenSource("t")); err == nil {
		t.Fatal("expected a missing-baseUrl error rather than a default-host call")
	}
}

// TestTerminalRunAbortLabelReachesGiteaEndpoint is the end-to-end proof, with
// NO provider seam overridden: the real Gitea backend is constructed from
// config and its actual HTTP traffic lands on the configured self-hosted host
// carrying the Gitea `Authorization: token <pat>` header. Any regression back
// to the GitHub provider would fail to reach this server at all.
func TestTerminalRunAbortLabelReachesGiteaEndpoint(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var auths []string
	var labelBody map[string][]int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/77"):
			_, _ = w.Write([]byte(`{"number":77,"title":"t","state":"open","labels":[]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(`[{"id":9,"name":"` + abortedRunLabel + `"}]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/77/labels"):
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&labelBody)
			mu.Unlock()
			_, _ = w.Write([]byte(`[{"id":9,"name":"` + abortedRunLabel + `"}]`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	// The GitHub arm is a tripwire: reaching it means the dispatch regressed.
	previousGitHub := newRunAbortLabelProvider
	newRunAbortLabelProvider = func(providers.TokenSource) workItemUpdater {
		t.Error("gitea-routed run-abort label constructed the GitHub provider")
		return fakeWorkItemUpdater(func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
			return providers.WorkItem{}, nil
		})
	}
	t.Cleanup(func() { newRunAbortLabelProvider = previousGitHub })

	cfg := giteaTerminalConfig(srv.URL)
	provider, err := newTerminalRunAbortLabelProviderForProject(cfg, apiv1.RepoRef{}, staticTerminalTokenSource("gitea-secret-pat"))
	if err != nil {
		t.Fatal(err)
	}
	repo := terminalRepositoryRefForProject(cfg, apiv1.RepoRef{})
	if _, err := provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: repo, ID: "77", AddLabels: []string{abortedRunLabel},
	}); err != nil {
		t.Fatalf("UpdateWorkItem against gitea: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("no request reached the gitea test server")
	}
	var sawLabelPost bool
	for _, p := range paths {
		if !strings.HasPrefix(p, "/api/v1/repos/acme/widgets") {
			t.Fatalf("request path %q is not the configured gitea repo route", p)
		}
		if p == "/api/v1/repos/acme/widgets/issues/77/labels" {
			sawLabelPost = true
		}
	}
	if !sawLabelPost {
		t.Fatalf("no label mutation reached gitea; paths = %v", paths)
	}
	for _, auth := range auths {
		if auth != "token gitea-secret-pat" {
			t.Fatalf("Authorization = %q, want gitea token scheme with the configured pat", auth)
		}
	}
	if got := labelBody["labels"]; len(got) != 1 || got[0] != 9 {
		t.Fatalf("label add body = %v, want the resolved %s id", got, abortedRunLabel)
	}
}

// TestTerminalBranchDeleteProviderRoutesByRepoKind covers the sibling terminal
// hook: branch cleanup had the same unconditional GitHub construction, so a
// Gitea instance's unmerged terminal branches were never actually deleted.
func TestTerminalBranchDeleteProviderRoutesByRepoKind(t *testing.T) {
	previousGitHub := newTerminalBranchDeleter
	newTerminalBranchDeleter = func(providers.TokenSource) providers.BranchDeleter {
		t.Error("gitea-routed terminal branch delete constructed the GitHub provider")
		return fakeBranchDeleter(func(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
			return providers.DeleteBranchResult{}, nil
		})
	}
	t.Cleanup(func() { newTerminalBranchDeleter = previousGitHub })

	previousGitea := newGiteaTerminalBranchDeleter
	var gotBaseURL, gotToken string
	newGiteaTerminalBranchDeleter = func(baseURL string, source providers.TokenSource) providers.BranchDeleter {
		gotBaseURL = baseURL
		token, err := source.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		gotToken = token
		return fakeBranchDeleter(func(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
			return providers.DeleteBranchResult{Deleted: true}, nil
		})
	}
	t.Cleanup(func() { newGiteaTerminalBranchDeleter = previousGitea })

	cfg := giteaTerminalConfig("https://gitea.example.test")
	deleter, err := newTerminalBranchDeleteProviderForProject(cfg, apiv1.RepoRef{}, staticTerminalTokenSource("gitea-pat"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deleter.DeleteBranch(context.Background(), providers.DeleteBranchRequest{
		Repository: terminalRepositoryRefForProject(cfg, apiv1.RepoRef{}), Name: "goobers/implementation/run",
	}); err != nil {
		t.Fatal(err)
	}
	if gotBaseURL != "https://gitea.example.test" || gotToken != "gitea-pat" {
		t.Fatalf("baseURL = %q, token = %q", gotBaseURL, gotToken)
	}
}

// TestBuildTerminalRunAbortLabelerUsesGiteaOnGiteaInstance walks the real
// builder (credential injector included) on a Gitea instance and asserts the
// label lands on the Gitea host, not api.github.com.
func TestBuildTerminalRunAbortLabelerUsesGiteaOnGiteaInstance(t *testing.T) {
	t.Setenv("GITEA_PR_TOKEN", "gitea-instance-token")

	var mu sync.Mutex
	var sawLabelPost bool
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/42/labels") {
			sawLabelPost = true
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/42"):
			_, _ = w.Write([]byte(`{"number":42,"title":"t","state":"open","labels":[]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(`[{"id":5,"name":"` + abortedRunLabel + `"}]`))
		default:
			_, _ = w.Write([]byte(`[{"id":5,"name":"` + abortedRunLabel + `"}]`))
		}
	}))
	t.Cleanup(srv.Close)

	previousGitHub := newRunAbortLabelProvider
	newRunAbortLabelProvider = func(providers.TokenSource) workItemUpdater {
		t.Error("gitea instance built the GitHub run-abort label provider")
		return fakeWorkItemUpdater(func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
			return providers.WorkItem{}, nil
		})
	}
	t.Cleanup(func() { newRunAbortLabelProvider = previousGitHub })

	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: "gitea",
		Owner:    "acme",
		Name:     "widgets",
		BaseURL:  srv.URL,
		Token:    instance.TokenRef{Env: "GITEA_PR_TOKEN"},
	}}}
	registrar := journal.NewRegistryScrubber()
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitea, Owner: "acme", Name: "widgets"}
	labelPR, err := buildTerminalRunAbortLabeler(cfg, project, registrar, nil)
	if err != nil {
		t.Fatal(err)
	}
	if labelPR == nil {
		t.Fatal("nil labeler")
	}
	if _, err := labelPR(context.Background(), providers.UpdateWorkItemRequest{
		Repository: terminalRepositoryRefForProject(cfg, apiv1.RepoRef{}), ID: "42", AddLabels: []string{abortedRunLabel},
	}); err != nil {
		t.Fatalf("label via gitea: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawLabelPost {
		t.Fatal("label mutation never reached the gitea server")
	}
	if gotAuth != "token gitea-instance-token" {
		t.Fatalf("Authorization = %q, want the instance gitea token", gotAuth)
	}
}
