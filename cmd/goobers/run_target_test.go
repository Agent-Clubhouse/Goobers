package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const targetedMergeReviewWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: merge-review
spec:
  gaggle: example
  triggers:
    - type: webhook
      events: [pull_request]
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: run a no-op local command
      run:
        workspace: scratch
        command: ["goobers", "__demo-provider", "curate"]
`

type targetedPullRequestFixture struct {
	server        *httptest.Server
	requiredToken string
	number        int
	state         string
	htmlURL       string
	status        int

	mu            sync.Mutex
	requests      int
	authorization string
}

func newTargetedPullRequestFixture(t *testing.T, number int, state, htmlURL, requiredToken string, status int) *targetedPullRequestFixture {
	t.Helper()
	fixture := &targetedPullRequestFixture{
		requiredToken: requiredToken,
		number:        number,
		state:         state,
		htmlURL:       htmlURL,
		status:        status,
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.requests++
		fixture.authorization = r.Header.Get("Authorization")
		requiredToken := fixture.requiredToken
		number := fixture.number
		state := fixture.state
		htmlURL := fixture.htmlURL
		status := fixture.status
		fixture.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/repos/your-org/your-repo/pulls/"+strconv.Itoa(number) {
			http.NotFound(w, r)
			return
		}
		if requiredToken != "" && r.Header.Get("Authorization") != "Bearer "+requiredToken {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"fixture rejection"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":   number,
			"state":    state,
			"html_url": htmlURL,
		})
	}))
	t.Cleanup(fixture.server.Close)

	previousProvider := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(provider *providers.GitHubProvider) {
			provider.BaseURL = fixture.server.URL
		})...)
	}
	t.Cleanup(func() { newGitHubProvider = previousProvider })
	return fixture
}

func (f *targetedPullRequestFixture) requestSnapshot() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.authorization
}

func (f *targetedPullRequestFixture) setState(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
}

func initTargetedMergeReviewDemo(t *testing.T, token string) string {
	t.Helper()
	t.Setenv("GOOBERS_GITHUB_TOKEN", token)
	t.Setenv("GOOBERS_OTHER_REPO_TOKEN", "unrelated-repository-token")
	root := initDemo(t)
	configPath := instance.NewLayout(root).ConfigFile()
	cfg, err := instance.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = append([]instance.RepoRef{{
		Provider: "github",
		Owner:    "other-org",
		Name:     "other-repo",
		Token:    instance.TokenRef{Env: "GOOBERS_OTHER_REPO_TOKEN"},
	}}, cfg.Repos...)
	if err := instance.WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	gaggleDir := filepath.Join(root, "config", "gaggles", "example")
	workflowPath := filepath.Join(gaggleDir, "workflows", "default-implement.yaml")
	if err = os.WriteFile(workflowPath, []byte(targetedMergeReviewWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(gaggleDir, "goobers")); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertNoTargetedRuns(t *testing.T, root string) {
	t.Helper()
	if count := targetedRunCount(t, root); count != 0 {
		t.Fatalf("run count = %d, want no run after rejected targeted request", count)
	}
}

func targetedRunCount(t *testing.T, root string) int {
	t.Helper()
	runsDir := instance.NewLayout(root).ForGaggle("example").RunsDir()
	entries, err := os.ReadDir(runsDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(entries)
}

func TestRunTargetRejectsExplicitZeroPR(t *testing.T) {
	if !flagWasSet([]string{"merge-review", "--pr", "0"}, "pr") {
		t.Fatal("flagWasSet did not detect an explicit --pr flag")
	}
}

func TestPullRequestURLMatchesConfiguredRepository(t *testing.T) {
	for _, test := range []struct {
		name string
		repo apiv1.RepoRef
		url  string
		want bool
	}{
		{
			name: "github",
			repo: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
			url:  "https://github.com/acme/web/pull/42",
			want: true,
		},
		{
			name: "github wrong host",
			repo: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
			url:  "https://example.invalid/acme/web/pull/42",
		},
		{
			name: "gitea",
			repo: apiv1.RepoRef{Provider: apiv1.ProviderGitea, BaseURL: "https://gitea.example.com", Owner: "acme", Name: "web"},
			url:  "https://gitea.example.com/acme/web/pulls/42",
			want: true,
		},
		{
			name: "ado dev azure",
			repo: apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "platform", Name: "web"},
			url:  "https://dev.azure.com/acme/platform/_git/web/pullrequest/42",
			want: true,
		},
		{
			name: "ado visual studio",
			repo: apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "platform", Name: "web"},
			url:  "https://acme.visualstudio.com/platform/_git/web/pullrequest/42",
			want: true,
		},
		{
			name: "ado wrong organization",
			repo: apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "platform", Name: "web"},
			url:  "https://dev.azure.com/other/platform/_git/web/pullrequest/42",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pullRequestURLMatchesRepository(test.repo, test.url, 42); got != test.want {
				t.Fatalf("pullRequestURLMatchesRepository(%q) = %v, want %v", test.url, got, test.want)
			}
		})
	}
}

func TestRunTargetedPullRequestCompletesWithExactTriggerReference(t *testing.T) {
	const token = "targeted-authorized-token"
	fixture := newTargetedPullRequestFixture(t, 3261, "open", "https://github.com/your-org/your-repo/pull/3261", token, http.StatusOK)
	root := initTargetedMergeReviewDemo(t, token)

	code, stdout, stderr := runArgs(t, "run", "merge-review", "--pr", "3261", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	runID := runIDFromRunStdout(t, stdout)
	reader, err := journal.OpenRead(filepath.Join(instance.NewLayout(root).ForGaggle("example").RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if identity.Trigger.Kind != journal.TriggerSignal || identity.Trigger.Ref != "github-webhook:pull_request#3261" {
		t.Fatalf("trigger = %+v, want exact targeted pull-request reference", identity.Trigger)
	}
	requests, authorization := fixture.requestSnapshot()
	if requests != 1 {
		t.Fatalf("provider requests = %d, want one fresh validation lookup", requests)
	}
	if authorization != "Bearer "+token {
		t.Fatalf("authorization = %q, want configured repository token", authorization)
	}
}

func TestRunTargetedPullRequestValidationUsesFreshProviderState(t *testing.T) {
	const token = "targeted-fresh-token"
	fixture := newTargetedPullRequestFixture(t, 3261, "open", "https://github.com/your-org/your-repo/pull/3261", token, http.StatusOK)
	root := initTargetedMergeReviewDemo(t, token)

	code, stdout, stderr := runArgs(t, "run", "merge-review", "--pr", "3261", root)
	if code != 0 {
		t.Fatalf("first run: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	fixture.setState("closed")
	code, stdout, stderr = runArgs(t, "run", "merge-review", "--pr", "3261", root)
	if code == 0 || !strings.Contains(stderr, "is closed") {
		t.Fatalf("second run: code = %d, stdout = %q, stderr = %q; want fresh closed-PR rejection", code, stdout, stderr)
	}
	if count := targetedRunCount(t, root); count != 1 {
		t.Fatalf("run count = %d, want only the first validated run", count)
	}
	requests, _ := fixture.requestSnapshot()
	if requests != 2 {
		t.Fatalf("provider requests = %d, want a fresh lookup for each CLI request", requests)
	}
}

func TestRunTargetedPullRequestRejectsProviderValidationFailures(t *testing.T) {
	for _, test := range []struct {
		name          string
		requestNumber int
		state         string
		htmlURL       string
		configToken   string
		requiredToken string
		status        int
		wantError     string
	}{
		{
			name:          "invalid",
			requestNumber: 404,
			state:         "open",
			htmlURL:       "https://github.com/your-org/your-repo/pull/404",
			configToken:   "authorized-token",
			requiredToken: "authorized-token",
			status:        http.StatusNotFound,
			wantError:     "status 404",
		},
		{
			name:          "closed",
			requestNumber: 3261,
			state:         "closed",
			htmlURL:       "https://github.com/your-org/your-repo/pull/3261",
			configToken:   "authorized-token",
			requiredToken: "authorized-token",
			status:        http.StatusOK,
			wantError:     "is closed",
		},
		{
			name:          "cross repository",
			requestNumber: 3261,
			state:         "open",
			htmlURL:       "https://github.com/other-org/other-repo/pull/3261",
			configToken:   "authorized-token",
			requiredToken: "authorized-token",
			status:        http.StatusOK,
			wantError:     "outside configured repository",
		},
		{
			name:          "unauthorized",
			requestNumber: 3261,
			state:         "open",
			htmlURL:       "https://github.com/your-org/your-repo/pull/3261",
			configToken:   "denied-token",
			requiredToken: "authorized-token",
			status:        http.StatusOK,
			wantError:     "status 403",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTargetedPullRequestFixture(t, test.requestNumber, test.state, test.htmlURL, test.requiredToken, test.status)
			root := initTargetedMergeReviewDemo(t, test.configToken)

			code, stdout, stderr := runArgs(t, "run", "merge-review", "--pr", strconv.Itoa(test.requestNumber), root)
			if code == 0 {
				t.Fatalf("run unexpectedly succeeded: stdout = %q, stderr = %q", stdout, stderr)
			}
			if !strings.Contains(stderr, test.wantError) {
				t.Fatalf("stderr = %q, want %q", stderr, test.wantError)
			}
			assertNoTargetedRuns(t, root)
			requests, _ := fixture.requestSnapshot()
			if requests != 1 {
				t.Fatalf("provider requests = %d, want one validation lookup", requests)
			}
		})
	}
}

func TestRunTargetAllowsQualifiedMergeReview(t *testing.T) {
	target, err := parseRunTarget("release/merge-review", "")
	if err != nil {
		t.Fatalf("parseRunTarget: %v", err)
	}
	if target.Gaggle != "release" || target.Workflow != "merge-review" {
		t.Fatalf("target = %+v", target)
	}
}

func TestRunTargetCLIRejectsInvalidTargetedPullRequests(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "zero", args: []string{"run", "merge-review", "--pr", "0", t.TempDir()}},
		{name: "negative", args: []string{"run", "merge-review", "--pr", "-1", t.TempDir()}},
		{name: "wrong workflow", args: []string{"run", "implement", "--pr", "42", t.TempDir()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := runArgs(t, test.args...)
			if code != 2 {
				t.Fatalf("code = %d, want usage error 2; stderr = %q", code, stderr)
			}
			if !strings.Contains(stderr, "positive pull request number") {
				t.Fatalf("stderr = %q, want actionable --pr error", stderr)
			}
		})
	}
}
