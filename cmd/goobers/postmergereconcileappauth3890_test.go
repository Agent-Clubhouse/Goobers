package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// postmergereconcileappauth3890_test.go is the #3890 regression, the sibling
// of #3885's applyverdictappauth3885_test.go.
//
// mergeStageProviderWithRecorder — the constructor post-merge-reconcile builds
// its merge/unpark provider with — hand-rolled its GitHub arm exactly the way
// apply-verdict's did before #3886, so it never reached
// newGitHubProviderForStage and therefore never applied
// providers.WithConfiguredLogin. Under GitHub App auth that left the provider
// with no declared identity, so AuthenticatedLogin (reached through
// reconcileOpenPullRequestParks -> unpark* -> the sibling-context author check)
// fell back to GET /user, which an installation token cannot call: 403
// "Resource not accessible by integration", and reconciliation is suppressed.
//
// These tests use the REAL constructor. A stub provider would have proved
// nothing about the constructor that was wrong.

// capturingRecorder is a journal mutation recorder that keeps what it was
// handed, so a test can prove the recorder actually reached the constructed
// provider rather than being dropped on the way through the seam.
type capturingRecorder struct {
	mu   sync.Mutex
	refs []providers.ExternalRef
}

func (r *capturingRecorder) RecordExternalRef(_ context.Context, ref providers.ExternalRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
}

func (r *capturingRecorder) recorded() []providers.ExternalRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]providers.ExternalRef(nil), r.refs...)
}

// reviewSubmitForge answers the one POST both backends' SubmitPullRequestReview
// makes, so the same call can prove recorder wiring on either forge.
func reviewSubmitForge(t *testing.T, wantPath string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != wantPath {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 7, "html_url": "https://forge.test/pulls/12#review-7", "commit_id": "deadbeef", "state": "APPROVED",
		})
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// The #3890 regression: post-merge-reconcile's REAL provider constructor,
// under App auth, must resolve its identity from repos[].auth.slug and never
// touch GET /user.
func TestPostMergeReconcileProviderUsesConfiguredAppLoginWithoutUserRequest(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")

	forge := newRecordingForge(t, "")

	provider, err := mergeStageProviderWithRecorder(root, repo, "ghs-installation-token", sidecarMutationRecorder{kind: "pr"})
	if err != nil {
		t.Fatalf("mergeStageProviderWithRecorder: %v", err)
	}
	githubProvider, ok := provider.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.GitHubProvider", provider)
	}
	githubProvider.BaseURL = forge.server.URL

	login, err := provider.AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "goobersbot[bot]" {
		t.Fatalf("AuthenticatedLogin = %q, want %q", login, "goobersbot[bot]")
	}
	if paths := forge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("provider made %d API request(s) %v, want none — the configured App login must be answered locally", len(paths), paths)
	}
}

// The other half of the contract: with no configured App slug (the PAT case)
// identity resolution must still go through GET /user.
func TestPostMergeReconcileProviderKeepsUserLookupForPAT(t *testing.T) {
	root := initDemo(t)
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	repo := githubRepoRefFromConfig(t, cfg)

	forge := newRecordingForge(t, "pat-user")

	provider, err := mergeStageProviderWithRecorder(root, repo, "pat-token", sidecarMutationRecorder{kind: "pr"})
	if err != nil {
		t.Fatalf("mergeStageProviderWithRecorder: %v", err)
	}
	githubProvider, ok := provider.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.GitHubProvider", provider)
	}
	githubProvider.BaseURL = forge.server.URL

	login, err := provider.AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "pat-user" {
		t.Fatalf("AuthenticatedLogin = %q, want %q", login, "pat-user")
	}
	if !forge.requested("/user") {
		t.Fatalf("requested paths = %v, want a GET /user lookup", forge.requestedPaths())
	}
}

// The constructor reaches the registered stage factories, and each arm still
// asks for exactly what it asked for before: the caller's own token and
// mutation recorder, PR-write capability, write mode, and the GitHub-only
// conditional-GET cache. ADO is included because the pre-#3890 switch had a
// `default:` arm that silently handed an ADO repo a GITHUB provider; routing
// through the seam sends it to the registered ADO factory instead.
func TestMergeStageProviderWithRecorderRoutesThroughStageSeam(t *testing.T) {
	recorder := &capturingRecorder{}
	for _, tc := range []struct {
		name       string
		kind       providers.ProviderKind
		repo       providers.RepositoryRef
		wantCached bool
	}{
		{
			name:       "github",
			kind:       providers.ProviderGitHub,
			repo:       providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
			wantCached: true,
		},
		{
			name: "gitea",
			kind: providers.ProviderGitea,
			repo: providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "acme", Name: "web"},
		},
		{
			name: "ado",
			kind: providers.ProviderADO,
			repo: providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "contoso", Project: "project", Name: "repo"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := stageProviderFactories[tc.kind]
			t.Cleanup(func() { stageProviderFactories[tc.kind] = previous })
			var got stageProviderConfig
			var called bool
			stageProviderFactories[tc.kind] = func(cfg stageProviderConfig) (providers.Provider, error) {
				got = cfg
				called = true
				return providers.NewGitHubProvider("stub"), nil
			}

			if _, err := mergeStageProviderWithRecorder(t.TempDir(), tc.repo, "stage-token", recorder); err != nil {
				t.Fatalf("mergeStageProviderWithRecorder: %v", err)
			}
			if !called {
				t.Fatalf("registered stage factory for %q was not called", tc.kind)
			}
			if got.capability != capability.GitHubPRWrite {
				t.Fatalf("capability = %q, want %q", got.capability, capability.GitHubPRWrite)
			}
			if got.token != "stage-token" {
				t.Fatalf("token = %q, want the caller's own capability token", got.token)
			}
			if got.readOnly {
				t.Fatal("readOnly = true, want false: post-merge reconcile mutates")
			}
			if got.cached != tc.wantCached {
				t.Fatalf("cached = %v, want %v", got.cached, tc.wantCached)
			}
			if got.mutationRecorder != providers.MutationRecorder(recorder) {
				t.Fatalf("mutationRecorder = %#v, want the caller's own recorder", got.mutationRecorder)
			}
			if got.mutationKind != "" {
				t.Fatalf("mutationKind = %q, want empty: this constructor supplies the recorder itself", got.mutationKind)
			}
			if got.openPR {
				t.Fatal("openPR = true, want false")
			}
		})
	}
}

// Recorder semantics survive the reroute on the GitHub arm: the caller's own
// recorder — not a sidecar recorder synthesized inside the seam — is what the
// constructed provider records through.
func TestMergeStageProviderRecordsGitHubMutationsThroughTheCallersRecorder(t *testing.T) {
	root := initDemo(t)
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	repo := githubRepoRefFromConfig(t, cfg)
	server, paths := reviewSubmitForge(t, "/repos/"+repo.Owner+"/"+repo.Name+"/pulls/12/reviews")

	recorder := &capturingRecorder{}
	provider, err := mergeStageProviderWithRecorder(root, repo, "pat-token", recorder)
	if err != nil {
		t.Fatalf("mergeStageProviderWithRecorder: %v", err)
	}
	githubProvider, ok := provider.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.GitHubProvider", provider)
	}
	githubProvider.BaseURL = server.URL

	if _, err := provider.SubmitPullRequestReview(context.Background(), providers.PullRequestReviewRequest{
		Repository: repo, PullID: "12", CommitSHA: "deadbeef", Body: "ok", Decision: providers.ReviewDecisionApproved,
	}); err != nil {
		t.Fatalf("SubmitPullRequestReview: %v (paths %v)", err, paths())
	}
	refs := recorder.recorded()
	if len(refs) != 1 {
		t.Fatalf("recorded %d ref(s) %+v, want exactly one", len(refs), refs)
	}
	if refs[0].Provider != providers.ProviderGitHub || refs[0].Operation != "review" {
		t.Fatalf("recorded ref = %+v, want a github review mutation", refs[0])
	}
}

// The same on the Gitea arm, which is uncached and takes the recorder through
// providers.WithGiteaMutationRecorder — a different option than the GitHub arm,
// so it needs its own proof that the reroute kept it wired.
func TestMergeStageProviderRecordsGiteaMutationsThroughTheCallersRecorder(t *testing.T) {
	root := initDemo(t)
	server, paths := reviewSubmitForge(t, "/api/v1/repos/your-org/your-repo/pulls/12/reviews")
	configureRemediationGitea(t, root, server.URL)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "your-org", Name: "your-repo"}

	recorder := &capturingRecorder{}
	provider, err := mergeStageProviderWithRecorder(root, repo, "gitea-token", recorder)
	if err != nil {
		t.Fatalf("mergeStageProviderWithRecorder: %v", err)
	}
	if _, ok := provider.(*providers.GiteaProvider); !ok {
		t.Fatalf("provider type = %T, want *providers.GiteaProvider", provider)
	}

	if _, err := provider.SubmitPullRequestReview(context.Background(), providers.PullRequestReviewRequest{
		Repository: repo, PullID: "12", CommitSHA: "deadbeef", Body: "ok", Decision: providers.ReviewDecisionApproved,
	}); err != nil {
		t.Fatalf("SubmitPullRequestReview: %v (paths %v)", err, paths())
	}
	refs := recorder.recorded()
	if len(refs) != 1 {
		t.Fatalf("recorded %d ref(s) %+v, want exactly one", len(refs), refs)
	}
	if refs[0].Provider != providers.ProviderGitea || refs[0].Operation != "review" {
		t.Fatalf("recorded ref = %+v, want a gitea review mutation", refs[0])
	}
}

// The Gitea arm must not pick up the GitHub arm's configured-login option:
// WithConfiguredLogin is a GitHub construct (GET /user is a GitHub endpoint),
// and Gitea resolves its own identity. Declaring an App slug on a GitHub repo
// in the same config must not leak into the Gitea provider.
func TestMergeStageProviderGiteaArmIsUnaffectedByGitHubAppSlug(t *testing.T) {
	root := initDemo(t)
	declareGitHubAppAuth(t, root, "goobersbot")
	server, _ := reviewSubmitForge(t, "/api/v1/unused")
	configureRemediationGitea(t, root, server.URL)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "your-org", Name: "your-repo"}

	provider, err := mergeStageProviderWithRecorder(root, repo, "gitea-token", sidecarMutationRecorder{kind: "pr"})
	if err != nil {
		t.Fatalf("mergeStageProviderWithRecorder: %v", err)
	}
	giteaProvider, ok := provider.(*providers.GiteaProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.GiteaProvider", provider)
	}
	if giteaProvider.BaseURL != server.URL+"/api/v1" {
		t.Fatalf("BaseURL = %q, want the configured Gitea base", giteaProvider.BaseURL)
	}
}
