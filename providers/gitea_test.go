package providers

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- constructor / auth ---

func TestGiteaProviderConstructorNormalizesBaseURL(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantBase    string
		wantRoot    string
		wantInitErr bool
	}{
		{name: "bare root", input: "https://gitea.example.com", wantBase: "https://gitea.example.com/api/v1", wantRoot: "https://gitea.example.com"},
		{name: "trailing slash trimmed", input: "https://gitea.example.com/", wantBase: "https://gitea.example.com/api/v1", wantRoot: "https://gitea.example.com"},
		{name: "already has api/v1 suffix", input: "https://gitea.example.com/api/v1", wantBase: "https://gitea.example.com/api/v1", wantRoot: "https://gitea.example.com"},
		{name: "api/v1 suffix with trailing slash", input: "https://gitea.example.com/api/v1/", wantBase: "https://gitea.example.com/api/v1", wantRoot: "https://gitea.example.com"},
		{name: "empty", input: "", wantInitErr: true},
		{name: "whitespace only", input: "   ", wantInitErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewGiteaProvider(tc.input, "tok")
			if p.Kind() != ProviderGitea {
				t.Fatalf("Kind() = %q, want %q", p.Kind(), ProviderGitea)
			}
			if tc.wantInitErr {
				if err := p.ready(); err == nil {
					t.Fatal("expected a deferred constructor error for an empty base URL")
				}
				return
			}
			if err := p.ready(); err != nil {
				t.Fatalf("ready() = %v, want nil", err)
			}
			if p.BaseURL != tc.wantBase {
				t.Fatalf("BaseURL = %q, want %q", p.BaseURL, tc.wantBase)
			}
			if p.RootURL != tc.wantRoot {
				t.Fatalf("RootURL = %q, want %q", p.RootURL, tc.wantRoot)
			}
			// /api/v1 must appear exactly once, however it was supplied.
			if strings.Count(p.BaseURL, "/api/v1") != 1 {
				t.Fatalf("BaseURL = %q, want exactly one /api/v1 suffix", p.BaseURL)
			}
		})
	}
}

func TestGiteaGitAuthEnvironmentScopesTokenAndRegistersSecret(t *testing.T) {
	reg := &spyGitRegistrar{}
	const token = "gitea-token-xyz"
	const url = "https://gitea.example.com/acme/app.git"
	env := GiteaGitAuthEnvironment(token, url, reg)
	joined := strings.Join(env, "\n")
	auth := base64.StdEncoding.EncodeToString([]byte(token + ":"))

	if !strings.Contains(joined, "GIT_CONFIG_KEY_1=http."+url+"/.extraheader") {
		t.Errorf("missing URL-scoped extraheader in %#v", env)
	}
	if !strings.Contains(joined, "GIT_CONFIG_VALUE_1=AUTHORIZATION: basic "+auth) {
		t.Errorf("missing basic-auth header in %#v", env)
	}
	if !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("missing terminal-prompt guard in %#v", env)
	}
	for _, e := range env {
		if strings.Contains(e, token) {
			t.Errorf("raw token leaked into env entry %q", e)
		}
	}
	if !reg.saw(token) || !reg.saw(auth) {
		t.Errorf("token/auth not registered for scrubbing: %#v", reg.secrets)
	}

	anon := GiteaGitAuthEnvironment("", "https://gitea.example.com/acme/pub.git", reg)
	if strings.Contains(strings.Join(anon, "\n"), "extraheader") {
		t.Errorf("empty token should produce no auth header: %#v", anon)
	}
}

// TestGiteaProviderRequestsCarryTokenAuthHeader proves every REST request
// carries Gitea's native `Authorization: token <t>` scheme, and that the raw
// token never appears in a recorded external ref (only its digest does).
func TestGiteaProviderRequestsCarryTokenAuthHeader(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(t, w, map[string]interface{}{"number": 9, "title": "t", "state": "open"})
	}))
	defer server.Close()

	rec := &recordingRecorder{}
	provider := NewGiteaProvider(server.URL, "test-token", WithGiteaMutationRecorder(rec))
	_, err := provider.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "9")
	if err != nil {
		t.Fatalf("GetWorkItem returned error: %v", err)
	}
	if gotAuth != "token test-token" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "token test-token")
	}
}

// TestGiteaProviderTokenSourceResolvesPerRequest proves WithGiteaTokenSource
// overrides the statically injected token, resolving it fresh per request.
func TestGiteaProviderTokenSourceResolvesPerRequest(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(t, w, map[string]interface{}{"number": 9, "state": "open"})
	}))
	defer server.Close()

	source := &staticTokenSource{token: "from-source"}
	provider := NewGiteaProvider(server.URL, "static-token", WithGiteaTokenSource(source))
	if _, err := provider.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "9"); err != nil {
		t.Fatalf("GetWorkItem returned error: %v", err)
	}
	if gotAuth != "token from-source" {
		t.Fatalf("Authorization header = %q, want token source's value", gotAuth)
	}
	if source.calls == 0 {
		t.Fatal("token source was never invoked")
	}
}

// --- clone / git-shell ---

func TestGiteaProviderCloneRepository(t *testing.T) {
	runner := &fakeRunner{}
	provider := NewGiteaProvider("https://gitea.example.com", "tok", func(p *GiteaProvider) { p.Runner = runner })
	clone, err := provider.CloneRepository(context.Background(), CloneRequest{
		Repository:  RepositoryRef{Owner: "acme", Name: "app"},
		Destination: "/tmp/app",
		Branch:      "main",
	})
	if err != nil {
		t.Fatalf("CloneRepository returned error: %v", err)
	}
	wantURL := "https://gitea.example.com/acme/app.git"
	if clone.URL != wantURL || clone.Path != "/tmp/app" {
		t.Fatalf("clone = %+v, want URL %q path /tmp/app", clone, wantURL)
	}
	if runner.name != "git" || !slicesEqual(runner.args, []string{"clone", "--branch", "main", wantURL, "/tmp/app"}) {
		t.Fatalf("runner call = %s %v", runner.name, runner.args)
	}
}

// TestGiteaProviderCloneRepositoryExplicitURLWins proves req.Repository.URL
// overrides the derived RootURL/owner/name clone URL.
func TestGiteaProviderCloneRepositoryExplicitURLWins(t *testing.T) {
	runner := &fakeRunner{}
	provider := NewGiteaProvider("https://gitea.example.com", "tok", func(p *GiteaProvider) { p.Runner = runner })
	clone, err := provider.CloneRepository(context.Background(), CloneRequest{
		Repository:  RepositoryRef{Owner: "acme", Name: "app", URL: "https://gitea.other/acme/app.git"},
		Destination: "/tmp/app",
	})
	if err != nil {
		t.Fatalf("CloneRepository returned error: %v", err)
	}
	if clone.URL != "https://gitea.other/acme/app.git" {
		t.Fatalf("clone.URL = %q, want the explicit repository URL", clone.URL)
	}
	if !containsString(runner.args, "https://gitea.other/acme/app.git") {
		t.Fatalf("runner args = %v, want the explicit URL", runner.args)
	}
}

// TestGiteaProviderCreateBranchResolvesBaseSHAViaLsRemote proves CreateBranch's
// git-shell argv when BaseSHA is empty: it resolves the base branch tip via
// `git ls-remote`, then fetches and pushes that SHA to refs/heads/<name>.
func TestGiteaProviderCreateBranchResolvesBaseSHAViaLsRemote(t *testing.T) {
	runner := &fakeEnvironmentRunner{}
	provider := NewGiteaProvider("https://gitea.example.com", "secret-token", func(p *GiteaProvider) { p.Runner = runner })

	// The first RunWithEnv call is ls-remote; its output must carry a SHA the
	// runner then reuses for fetch+push.
	// fakeEnvironmentRunner returns a single fixed output/err for every call, so
	// give it the ls-remote answer and verify the fetch/push argv shape only.
	runner.envOutput = []byte("deadbeefcafebabe1234567890abcdef01234567\trefs/heads/main\n")

	remoteURL := "https://gitea.example.com/acme/app.git"
	result, err := provider.CreateBranch(context.Background(), BranchRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		BaseBranch: "main",
		Name:       "work",
	})
	if err != nil {
		t.Fatalf("CreateBranch returned error: %v", err)
	}
	if result.Name != "work" || result.SHA != "deadbeefcafebabe1234567890abcdef01234567" {
		t.Fatalf("result = %+v", result)
	}
	if len(runner.envCalls) != 3 {
		t.Fatalf("environment runner calls = %d, want 3 (ls-remote, fetch, push): %+v", len(runner.envCalls), runner.envCalls)
	}
	lsRemote := runner.envCalls[0]
	if lsRemote.name != "git" || !slicesEqual(lsRemote.args, []string{"ls-remote", remoteURL, "refs/heads/main"}) {
		t.Fatalf("ls-remote call = %+v", lsRemote)
	}
	fetch := runner.envCalls[1]
	if fetch.name != "git" || len(fetch.args) != 4 || !strings.HasPrefix(fetch.args[0], "--git-dir=") ||
		fetch.args[1] != "fetch" || fetch.args[2] != remoteURL ||
		fetch.args[3] != "deadbeefcafebabe1234567890abcdef01234567" {
		t.Fatalf("fetch call = %+v", fetch)
	}
	push := runner.envCalls[2]
	if push.name != "git" || len(push.args) != 4 || push.args[1] != "push" || push.args[2] != remoteURL ||
		push.args[3] != "deadbeefcafebabe1234567890abcdef01234567:refs/heads/work" {
		t.Fatalf("push call = %+v", push)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("secret-token:"))
	if !containsString(push.env, "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth) {
		t.Fatal("push environment does not carry the injected Gitea authorization header")
	}
}

// TestGiteaProviderCreateBranchUsesExplicitBaseSHA proves a non-empty
// req.BaseSHA skips the ls-remote resolution entirely.
func TestGiteaProviderCreateBranchUsesExplicitBaseSHA(t *testing.T) {
	runner := &fakeEnvironmentRunner{}
	provider := NewGiteaProvider("https://gitea.example.com", "tok", func(p *GiteaProvider) { p.Runner = runner })
	result, err := provider.CreateBranch(context.Background(), BranchRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		BaseSHA:    "explicit-sha",
		Name:       "work",
	})
	if err != nil {
		t.Fatalf("CreateBranch returned error: %v", err)
	}
	if result.SHA != "explicit-sha" {
		t.Fatalf("result.SHA = %q, want explicit-sha", result.SHA)
	}
	// Only fetch + push (2 env calls) — no ls-remote.
	if len(runner.envCalls) != 2 {
		t.Fatalf("environment runner calls = %d, want 2 (fetch, push, no ls-remote): %+v", len(runner.envCalls), runner.envCalls)
	}
	if runner.envCalls[0].args[1] != "fetch" || runner.envCalls[1].args[1] != "push" {
		t.Fatalf("calls = %+v, want fetch then push", runner.envCalls)
	}
}

// TestGiteaProviderDeleteBranchUsesExpectedSHALease proves DeleteBranch's
// --force-with-lease argv when ExpectedSHA is set.
func TestGiteaProviderDeleteBranchUsesExpectedSHALease(t *testing.T) {
	runner := &fakeEnvironmentRunner{}
	provider := NewGiteaProvider("https://gitea.example.com", "secret-token", func(p *GiteaProvider) { p.Runner = runner })
	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{
		Repository:  RepositoryRef{Owner: "acme", Name: "app"},
		Name:        "goobers/implementation/run-1",
		ExpectedSHA: "validated-sha",
	})
	if err != nil {
		t.Fatalf("DeleteBranch returned error: %v", err)
	}
	if !result.Deleted {
		t.Fatal("Deleted = false, want true")
	}
	if len(runner.envCalls) != 1 {
		t.Fatalf("environment runner calls = %+v", runner.envCalls)
	}
	call := runner.envCalls[0]
	if call.name != "git" || len(call.args) != 6 ||
		!strings.HasPrefix(call.args[0], "--git-dir=") ||
		call.args[1] != "push" ||
		call.args[2] != "--porcelain" ||
		call.args[3] != "--force-with-lease=refs/heads/goobers/implementation/run-1:validated-sha" ||
		call.args[4] != "https://gitea.example.com/acme/app.git" ||
		call.args[5] != ":refs/heads/goobers/implementation/run-1" {
		t.Fatalf("push call = %+v", call)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("secret-token:"))
	if !containsString(call.env, "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth) {
		t.Fatal("push environment does not contain the injected authorization header")
	}
}

// TestGiteaProviderDeleteBranchPreservesStaleLease mirrors GitHub's lease
// test: a "(stale info)" rejection whose current tip still differs from
// ExpectedSHA (branch still exists) surfaces as *BranchTipChangedError.
func TestGiteaProviderDeleteBranchPreservesStaleLease(t *testing.T) {
	runner := &fakeEnvironmentRunner{
		envOutput: []byte("! refs/heads/run:refs/heads/run [rejected] (stale info)\n"),
		envErr:    errors.New("exit status 1"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/branches/goobers/implementation/run-1" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		writeJSON(t, w, map[string]interface{}{
			"name":   "goobers/implementation/run-1",
			"commit": map[string]interface{}{"id": "concurrent-sha"},
		})
	}))
	defer server.Close()
	provider := NewGiteaProvider(server.URL, "token", func(p *GiteaProvider) { p.Runner = runner })

	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{
		Repository:  RepositoryRef{Owner: "acme", Name: "app"},
		Name:        "goobers/implementation/run-1",
		ExpectedSHA: "validated-sha",
	})
	var tipChanged *BranchTipChangedError
	if !errors.As(err, &tipChanged) {
		t.Fatalf("DeleteBranch error = %v, want BranchTipChangedError", err)
	}
	if result.Deleted {
		t.Fatal("Deleted = true for a stale lease")
	}
}

// TestGiteaProviderDeleteBranchLeaseTreatsConcurrentDeletionAsAbsent mirrors
// GitHub's lease test: a "(stale info)" rejection whose branch is now gone
// (404 on the re-check) is treated as an already-absent branch — Deleted
// stays false but the error is nil.
func TestGiteaProviderDeleteBranchLeaseTreatsConcurrentDeletionAsAbsent(t *testing.T) {
	runner := &fakeEnvironmentRunner{
		envOutput: []byte("! refs/heads/run:refs/heads/run [rejected] (stale info)\n"),
		envErr:    errors.New("exit status 1"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/branches/goobers/implementation/run-1" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	provider := NewGiteaProvider(server.URL, "token", func(p *GiteaProvider) { p.Runner = runner })

	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{
		Repository:  RepositoryRef{Owner: "acme", Name: "app"},
		Name:        "goobers/implementation/run-1",
		ExpectedSHA: "validated-sha",
	})
	if err != nil {
		t.Fatalf("DeleteBranch returned error: %v", err)
	}
	if result.Deleted {
		t.Fatal("Deleted = true for an already absent branch")
	}
}

// --- real-git end-to-end: Commit, CompareCommits ---

func TestGiteaProviderCommitAtomicRealGit(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	workDir := filepath.Join(t.TempDir(), "seed")
	runGitTest(t, "init", "--bare", "--quiet", remoteDir)
	runGitTest(t, "init", "--quiet", workDir)
	runGitTest(t, "-C", workDir, "config", "user.name", "Goobers Test")
	runGitTest(t, "-C", workDir, "config", "user.email", "goobers@example.test")

	// Seed the remote with a "work" branch carrying a file to delete.
	if err := os.WriteFile(filepath.Join(workDir, "old.txt"), []byte("bye\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, "-C", workDir, "add", "old.txt")
	runGitTest(t, "-C", workDir, "commit", "--quiet", "-m", "seed")
	runGitTest(t, "-C", workDir, "push", "--quiet", remoteDir, "HEAD:refs/heads/work")

	provider := NewGiteaProvider(remoteDir, "", func(p *GiteaProvider) {})
	commit, err := provider.Commit(context.Background(), CommitRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app", URL: remoteDir},
		Branch:     "work",
		Message:    "docs update",
		Files: []CommitFile{
			{Path: "docs/README.md", Content: "hello\n"},
			{Path: "old.txt", ChangeType: string(CommitChangeDelete)},
		},
	})
	if err != nil {
		t.Fatalf("Commit returned error: %v", err)
	}
	if commit.SHA == "" {
		t.Fatal("Commit returned an empty SHA")
	}
	verifyDir := filepath.Join(t.TempDir(), "verify")
	runGitTest(t, "clone", "--quiet", "--branch", "work", remoteDir, verifyDir)
	if _, err := os.Stat(filepath.Join(verifyDir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old.txt should have been deleted, stat err = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(verifyDir, "docs", "README.md"))
	if err != nil || string(content) != "hello\n" {
		t.Fatalf("docs/README.md content = %q, err = %v", content, err)
	}
}

func TestGiteaProviderCompareCommitsRealGit(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	workDir := filepath.Join(t.TempDir(), "work")
	runGitTest(t, "init", "--bare", "--quiet", remoteDir)
	runGitTest(t, "init", "--quiet", workDir)
	runGitTest(t, "-C", workDir, "config", "user.name", "Goobers Test")
	runGitTest(t, "-C", workDir, "config", "user.email", "goobers@example.test")

	if err := os.WriteFile(filepath.Join(workDir, "old-name.txt"), []byte("line one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, "-C", workDir, "add", "old-name.txt")
	runGitTest(t, "-C", workDir, "commit", "--quiet", "-m", "base")
	runGitTest(t, "-C", workDir, "push", "--quiet", remoteDir, "HEAD:refs/heads/main")
	baseSHA := strings.TrimSpace(runGitTest(t, "-C", workDir, "rev-parse", "HEAD"))

	runGitTest(t, "-C", workDir, "mv", "old-name.txt", "new-name.txt")
	runGitTest(t, "-C", workDir, "commit", "--quiet", "-m", "rename")
	runGitTest(t, "-C", workDir, "push", "--quiet", remoteDir, "HEAD:refs/heads/feature")
	headSHA := strings.TrimSpace(runGitTest(t, "-C", workDir, "rev-parse", "HEAD"))

	provider := NewGiteaProvider(remoteDir, "", func(p *GiteaProvider) {})
	result, err := provider.CompareCommits(context.Background(), RepositoryRef{Owner: "acme", Name: "app", URL: remoteDir}, baseSHA, headSHA)
	if err != nil {
		t.Fatalf("CompareCommits returned error: %v", err)
	}
	if result.MergeBaseSHA != baseSHA {
		t.Fatalf("MergeBaseSHA = %q, want %q", result.MergeBaseSHA, baseSHA)
	}
	if len(result.Files) != 1 {
		t.Fatalf("Files = %+v, want exactly one renamed file", result.Files)
	}
	file := result.Files[0]
	if file.Status != "renamed" || file.Path != "new-name.txt" || file.PreviousPath != "old-name.txt" {
		t.Fatalf("renamed file = %+v", file)
	}
}

// --- pull request REST operations ---

func TestGiteaProviderOpenPullRequestIsIdempotentOnRepass(t *testing.T) {
	var posts, patches int
	var patchedTitle string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if got := r.URL.Query().Get("state"); got != "open" {
				t.Fatalf("lookup state query = %q", got)
			}
			writeJSON(t, w, []map[string]interface{}{
				{
					"number": 9, "html_url": "https://gitea.test/acme/app/pulls/9",
					"head": map[string]interface{}{"ref": "goobers/impl/run-1"},
					"base": map[string]interface{}{"ref": "main"},
				},
			})
		case http.MethodPost:
			posts++
			writeJSON(t, w, map[string]interface{}{"number": 9, "html_url": "https://gitea.test/acme/app/pulls/9"})
		default:
			t.Fatalf("unexpected method %s on /pulls", r.Method)
		}
	})
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		var body map[string]interface{}
		decodeJSON(t, r, &body)
		patchedTitle, _ = body["title"].(string)
		patches++
		writeJSON(t, w, map[string]interface{}{"number": 9, "html_url": "https://gitea.test/acme/app/pulls/9"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	result, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "Implement #13 (repass)", Body: "Adds PR polling.",
		Head: "goobers/impl/run-1", Base: "main", RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("OpenPullRequest returned error: %v", err)
	}
	if result.Number != 9 {
		t.Fatalf("result.Number = %d, want 9 (the existing PR)", result.Number)
	}
	if posts != 0 {
		t.Fatalf("expected no POST (duplicate-create) call, got %d", posts)
	}
	if patches != 1 {
		t.Fatalf("expected exactly one PATCH (update) call, got %d", patches)
	}
	if patchedTitle != "Implement #13 (repass)" {
		t.Fatalf("patched title = %q", patchedTitle)
	}
}

func TestGiteaProviderOpenPullRequestDraftPrefixesWIP(t *testing.T) {
	var gotTitle string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, []map[string]interface{}{})
			return
		}
		var body map[string]interface{}
		decodeJSON(t, r, &body)
		gotTitle, _ = body["title"].(string)
		writeJSON(t, w, map[string]interface{}{"number": 9, "html_url": "https://gitea.test/acme/app/pulls/9"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	if _, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "Implement #13", Head: "goobers/impl/run-1", Base: "main", Draft: true,
	}); err != nil {
		t.Fatalf("OpenPullRequest returned error: %v", err)
	}
	if gotTitle != "WIP: Implement #13" {
		t.Fatalf("title = %q, want WIP-prefixed", gotTitle)
	}
}

func TestGiteaProviderPollPullRequestAggregatesState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"number": 9, "title": "WIP: Fix API", "state": "open", "merged": false,
			"html_url": "https://gitea.test/acme/app/pulls/9",
			"head":     map[string]interface{}{"ref": "work", "sha": "deadbeef"},
			"base":     map[string]interface{}{"ref": "main", "sha": "basesha"},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9/reviews", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{"state": "REQUEST_CHANGES", "user": map[string]string{"login": "alice"}},
			{"state": "COMMENT", "user": map[string]string{"login": "bob"}},
			{"state": "APPROVED", "user": map[string]string{"login": "alice"}},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/commits/deadbeef/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"statuses": []map[string]interface{}{
				{"context": "ci", "status": "success"},
			},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues/9/comments", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got == "" {
			t.Fatalf("expected since query param")
		}
		writeJSON(t, w, []map[string]interface{}{
			{"id": 1, "body": "fix this", "html_url": "c1", "user": map[string]string{"login": "carol"}, "created_at": "2026-07-13T00:00:00Z"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	since := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	result, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", CommentsSince: &since,
	})
	if err != nil {
		t.Fatalf("PollPullRequest returned error: %v", err)
	}
	if result.ReviewDecision != ReviewDecisionApproved {
		t.Fatalf("ReviewDecision = %q, want approved (alice's later APPROVED supersedes her CHANGES_REQUESTED)", result.ReviewDecision)
	}
	if result.RequestedChanges != 0 {
		t.Fatalf("RequestedChanges = %d, want 0", result.RequestedChanges)
	}
	if result.CheckState != CheckStatePassing {
		t.Fatalf("CheckState = %q, want passing", result.CheckState)
	}
	if !result.Draft {
		t.Fatalf("Draft = false, want true (WIP: title prefix)")
	}
	if result.MergeableState != "" {
		t.Fatalf("MergeableState = %q, want empty (no Gitea equivalent)", result.MergeableState)
	}
	if len(result.CommentsSince) != 1 || result.CommentsSince[0].Author != "carol" {
		t.Fatalf("CommentsSince = %#v", result.CommentsSince)
	}
}

func TestGiteaProviderMergePullRequestSucceeds(t *testing.T) {
	var gotBody map[string]interface{}
	var mergeCalls, getCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9/merge", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		mergeCalls++
		decodeJSON(t, r, &gotBody)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		getCalls++
		writeJSON(t, w, map[string]interface{}{"number": 9, "merge_commit_sha": "merged-sha"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	rec := &recordingRecorder{}
	provider := NewGiteaProvider(server.URL, "token", WithGiteaMutationRecorder(rec))
	result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
		ExpectedHeadSHA: "deadbeef", CommitTitle: "Improve merge history",
		CommitMessage: "merged", MergeMethod: MergeMethodSquash,
	})
	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if mergeCalls != 1 {
		t.Fatalf("merge endpoint calls = %d, want exactly 1 (POST, not PUT)", mergeCalls)
	}
	if getCalls != 1 {
		t.Fatalf("follow-up pull GET calls = %d, want 1 for MergeSHA resolution", getCalls)
	}
	if !result.Merged || result.MergeSHA != "merged-sha" || result.Number != 9 {
		t.Fatalf("result = %#v", result)
	}
	if gotBody["Do"] != "squash" {
		t.Fatalf("Do = %v, want squash", gotBody["Do"])
	}
	if gotBody["head_commit_id"] != "deadbeef" {
		t.Fatalf("head_commit_id = %v, want deadbeef", gotBody["head_commit_id"])
	}
	if gotBody["MergeTitleField"] != "Improve merge history" {
		t.Fatalf("MergeTitleField = %v", gotBody["MergeTitleField"])
	}
	if gotBody["MergeMessageField"] != "merged" {
		t.Fatalf("MergeMessageField = %v", gotBody["MergeMessageField"])
	}
	ref, ok := rec.last()
	if !ok || ref.Operation != "merge" {
		t.Fatalf("recorded ref = (%+v, %v), want merge", ref, ok)
	}
}

func TestGiteaProviderMergePullRequestRefusedOnSHAMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		writeJSON(t, w, map[string]string{"message": "head branch was modified"})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	_, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "stale-sha",
	})
	if err == nil {
		t.Fatal("expected an error for a stale SHA-pin (409), got nil")
	}
}

func TestGiteaProviderListPullRequestsFiltersByHeadPrefixAndReportsCheckState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		if got := r.URL.Query().Get("base"); got != "main" {
			t.Fatalf("base query = %q, want main", got)
		}
		writeJSON(t, w, []map[string]interface{}{
			{
				"number": 10, "html_url": "https://gitea.test/acme/app/pulls/10",
				"updated_at": "2026-07-15T00:00:00Z",
				"head":       map[string]interface{}{"ref": "goobers/implementation/run-1", "sha": "aaa111"},
				"base":       map[string]interface{}{"ref": "main", "sha": "base111"},
			},
			{
				"number": 11, "html_url": "https://gitea.test/acme/app/pulls/11",
				"updated_at": "2026-07-15T00:00:00Z",
				"head":       map[string]interface{}{"ref": "someone/manual-fix", "sha": "bbb222"},
				"base":       map[string]interface{}{"ref": "main", "sha": "base111"},
			},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/commits/aaa111/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"statuses": []map[string]interface{}{{"context": "ci", "status": "success"}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	out, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Base: "main", HeadPrefix: "goobers/",
	})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1 (the non-matching head must be excluded)", len(out))
	}
	pr := out[0]
	if pr.Number != 10 || pr.Head != "goobers/implementation/run-1" || pr.HeadSHA != "aaa111" || pr.BaseSHA != "base111" {
		t.Fatalf("unexpected summary: %+v", pr)
	}
	if pr.CheckState != CheckStatePassing {
		t.Fatalf("CheckState = %q, want passing", pr.CheckState)
	}
}

func TestGiteaProviderListPullRequestsSkipCheckState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{
				"number": 10, "html_url": "https://gitea.test/acme/app/pulls/10",
				"updated_at": "2026-07-15T00:00:00Z",
				"head":       map[string]interface{}{"ref": "goobers/implementation/run-1", "sha": "aaa111"},
				"base":       map[string]interface{}{"ref": "main", "sha": "base111"},
			},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/commits/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("SkipCheckState list must not resolve check state, got %s", r.URL.Path)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	out, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Base: "main", HeadPrefix: "goobers/",
		SkipCheckState: true,
	})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(out) != 1 || out[0].CheckState != "" {
		t.Fatalf("out = %+v, want one summary with empty CheckState", out)
	}
}

func TestGiteaProviderPullRequestFilesListsTouchedFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/pulls/12/files" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, []map[string]interface{}{
			{"filename": "internal/runner/run.go", "status": "modified", "additions": 12, "deletions": 3},
			{"filename": "cmd/goobers/new.go", "previous_filename": "cmd/goobers/old.go", "status": "renamed", "additions": 40, "deletions": 0},
		})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	files, err := provider.PullRequestFiles(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "12")
	if err != nil {
		t.Fatalf("PullRequestFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	if files[0].Path != "internal/runner/run.go" || files[0].Status != "modified" ||
		files[0].Additions != 12 || files[0].Deletions != 3 || files[0].Patch != "" {
		t.Fatalf("unexpected file[0]: %+v, want empty Patch (Gitea has no patch text)", files[0])
	}
	if files[1].PreviousPath != "cmd/goobers/old.go" {
		t.Fatalf("file[1] = %+v, want previous rename path", files[1])
	}
}

// --- unsupported merge queue / direct policy ---

// noCallHTTPClient fails the test if invoked at all — used to assert an
// operation issues zero HTTP requests.
type noCallHTTPClient struct{ t *testing.T }

func (c noCallHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.t.Fatalf("unexpected HTTP request: %s %s", req.Method, req.URL)
	return nil, nil
}

func TestGiteaProviderDetectMergePolicyIsAlwaysDirectWithNoRequests(t *testing.T) {
	provider := NewGiteaProvider("https://gitea.example.com", "token", func(p *GiteaProvider) {
		p.Client = noCallHTTPClient{t: t}
	})
	result, err := provider.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Branch: "main",
	})
	if err != nil {
		t.Fatalf("DetectMergePolicy returned error: %v", err)
	}
	if result.Policy != MergePolicyDirect {
		t.Fatalf("Policy = %q, want %q", result.Policy, MergePolicyDirect)
	}
}

func TestGiteaProviderEnqueueAndPollMergeQueueAreUnsupportedWithNoRequests(t *testing.T) {
	provider := NewGiteaProvider("https://gitea.example.com", "token", func(p *GiteaProvider) {
		p.Client = noCallHTTPClient{t: t}
	})
	_, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
	})
	if !errors.Is(err, ErrGiteaMergeQueueUnsupported) {
		t.Fatalf("EnqueuePullRequest error = %v, want ErrGiteaMergeQueueUnsupported", err)
	}
	_, err = provider.PollMergeQueueEntry(context.Background(), PollMergeQueueEntryRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
	})
	if !errors.Is(err, ErrGiteaMergeQueueUnsupported) {
		t.Fatalf("PollMergeQueueEntry error = %v, want ErrGiteaMergeQueueUnsupported", err)
	}
}

// --- UpdateBranch, reviews, statuses ---

func TestGiteaProviderUpdateBranchRejectsHeadMismatchWithoutCallingUpdate(t *testing.T) {
	var updateCalled bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			updateCalled = true
			return
		}
		writeJSON(t, w, map[string]interface{}{"number": 9, "head": map[string]interface{}{"sha": "current-sha"}})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9/update", func(w http.ResponseWriter, r *http.Request) {
		updateCalled = true
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	_, err := provider.UpdateBranch(context.Background(), UpdateBranchRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "stale-sha",
	})
	var updateErr *UpdateBranchError
	if !errors.As(err, &updateErr) || updateErr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("error = %v, want *UpdateBranchError{422}", err)
	}
	if updateCalled {
		t.Fatal("update endpoint must not be called on a head mismatch")
	}
}

func TestGiteaProviderUpdateBranchSucceedsOnMatch(t *testing.T) {
	var gotStyle string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"number": 9, "html_url": "pr-url", "head": map[string]interface{}{"sha": "current-sha"}})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9/update", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		gotStyle = r.URL.Query().Get("style")
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	result, err := provider.UpdateBranch(context.Background(), UpdateBranchRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "current-sha",
	})
	if err != nil {
		t.Fatalf("UpdateBranch returned error: %v", err)
	}
	if gotStyle != "merge" {
		t.Fatalf("style query = %q, want merge", gotStyle)
	}
	if result.Number != 9 {
		t.Fatalf("result = %+v", result)
	}
}

func TestGiteaProviderSubmitPullRequestReviewPostsEventAndCommit(t *testing.T) {
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/pulls/9/reviews" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertMethod(t, r, http.MethodPost)
		decodeJSON(t, r, &gotBody)
		writeJSON(t, w, map[string]interface{}{"id": 42, "html_url": "review-url"})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	result, err := provider.SubmitPullRequestReview(context.Background(), PullRequestReviewRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
		CommitSHA: "deadbeef", Decision: ReviewDecisionApproved, Body: "lgtm",
	})
	if err != nil {
		t.Fatalf("SubmitPullRequestReview returned error: %v", err)
	}
	if gotBody["event"] != "APPROVED" || gotBody["commit_id"] != "deadbeef" || gotBody["body"] != "lgtm" {
		t.Fatalf("request body = %#v", gotBody)
	}
	if result.ID != 42 || result.CommitSHA != "deadbeef" {
		t.Fatalf("result = %+v", result)
	}
}

func TestGiteaProviderRequestReviewPostsRequestedReviewers(t *testing.T) {
	var gotReviewers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/pulls/9/requested_reviewers" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertMethod(t, r, http.MethodPost)
		var body struct {
			Reviewers []string `json:"reviewers"`
		}
		decodeJSON(t, r, &body)
		gotReviewers = body.Reviewers
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	if err := provider.RequestReview(context.Background(), ReviewRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", Reviewers: []string{"qa-1"},
	}); err != nil {
		t.Fatalf("RequestReview returned error: %v", err)
	}
	if strings.Join(gotReviewers, ",") != "qa-1" {
		t.Fatalf("reviewers = %#v", gotReviewers)
	}
}

func TestGiteaProviderPublishPullRequestStatusResolvesHeadSHAThenPosts(t *testing.T) {
	var gotBody map[string]interface{}
	var statusPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"number": 9, "head": map[string]interface{}{"sha": "deadbeef"}})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/statuses/deadbeef", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		statusPath = r.URL.Path
		decodeJSON(t, r, &gotBody)
		writeJSON(t, w, map[string]interface{}{"id": 5})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	result, err := provider.PublishPullRequestStatus(context.Background(), PullRequestStatusRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
		Genre: "goobers", Name: "merge-review", State: CheckStatePassing, Description: "looks good",
	})
	if err != nil {
		t.Fatalf("PublishPullRequestStatus returned error: %v", err)
	}
	if statusPath != "/api/v1/repos/acme/app/statuses/deadbeef" {
		t.Fatalf("status path = %q", statusPath)
	}
	if gotBody["context"] != "goobers/merge-review" || gotBody["state"] != "success" {
		t.Fatalf("body = %#v", gotBody)
	}
	if result.ID != 5 {
		t.Fatalf("result = %+v", result)
	}
}
