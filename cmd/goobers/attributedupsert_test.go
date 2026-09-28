package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// The sticky upserts below (the merge-review status comment and the
// respond-to-findings account) relist after they write and update the
// canonical comment again when it does not hold the text they wrote. Under
// run attribution the stored body always carries the provider's footer, so an
// exact comparison sent one redundant update on every run. These tests pin the
// number of writes per run with attribution on, as every daemon stage runs.

var writeRouteNumber = regexp.MustCompile(`/\d+`)

// providerWriteCounter counts write requests by method and route shape, with
// numeric path segments folded to {n}.
type providerWriteCounter struct {
	mu     sync.Mutex
	writes map[string]int
}

func (c *providerWriteCounter) record(r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writes == nil {
		c.writes = map[string]int{}
	}
	c.writes[r.Method+" "+writeRouteNumber.ReplaceAllString(r.URL.Path, "/{n}")]++
}

// take returns the writes recorded since the previous take.
func (c *providerWriteCounter) take() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	writes := c.writes
	c.writes = nil
	if writes == nil {
		writes = map[string]int{}
	}
	return writes
}

// countingHTTPClient records every request before sending it.
type countingHTTPClient struct {
	counter *providerWriteCounter
	inner   *http.Client
}

func (c countingHTTPClient) Do(r *http.Request) (*http.Response, error) {
	c.counter.record(r)
	return c.inner.Do(r)
}

func assertProviderWrites(t *testing.T, label string, got, want map[string]int) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("%s: writes = %v, want %v", label, got, want)
	}
}

func testRunAttribution(task string) providers.Attribution {
	return providers.Attribution{
		Gaggle: "goobers", Workflow: "test-workflow", Task: task, Goober: "deterministic", Run: "run-attributed",
	}
}

// fakeGiteaComments serves the Gitea issue-comment calls the sticky upserts
// make for pull request 77 of your-org/your-repo, storing bodies verbatim and
// counting writes.
type fakeGiteaComments struct {
	t        *testing.T
	mu       sync.Mutex
	login    string
	nextID   int64
	comments []map[string]any
	writes   providerWriteCounter
}

func newFakeGiteaComments(t *testing.T, login string) (*fakeGiteaComments, *httptest.Server) {
	t.Helper()
	fake := &fakeGiteaComments{t: t, login: login}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeGiteaComments) serve(w http.ResponseWriter, r *http.Request) {
	f.writes.record(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	const prefix = "/api/v1/repos/your-org/your-repo/issues/"
	commentID, isCommentRoute := strings.CutPrefix(r.URL.Path, prefix+"comments/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
		writeFakeJSON(w, map[string]string{"login": f.login})
	case r.Method == http.MethodGet && r.URL.Path == prefix+"77":
		writeFakeJSON(w, map[string]any{
			"id": 77, "number": 77, "title": "Reviewed PR", "state": "open",
			"html_url": "https://gitea.test/your-org/your-repo/pulls/77",
		})
	case r.Method == http.MethodGet && r.URL.Path == prefix+"77/comments":
		writeFakeJSON(w, f.comments)
	case r.Method == http.MethodPost && r.URL.Path == prefix+"77/comments":
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			f.t.Errorf("decode Gitea comment: %v", err)
		}
		f.nextID++
		comment := map[string]any{"id": f.nextID, "body": request["body"], "user": map[string]string{"login": f.login}}
		f.comments = append(f.comments, comment)
		writeFakeJSON(w, comment)
	case isCommentRoute && (r.Method == http.MethodPatch || r.Method == http.MethodDelete):
		f.editComment(w, r, commentID)
	default:
		f.t.Errorf("unexpected Gitea request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

// editComment applies a PATCH or DELETE to one stored comment. Callers hold f.mu.
func (f *fakeGiteaComments) editComment(w http.ResponseWriter, r *http.Request, rawID string) {
	id, _ := strconv.ParseInt(rawID, 10, 64)
	for i, comment := range f.comments {
		if comment["id"] != id {
			continue
		}
		if r.Method == http.MethodDelete {
			f.comments = append(f.comments[:i], f.comments[i+1:]...)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			f.t.Errorf("decode Gitea comment edit: %v", err)
		}
		comment["body"] = request["body"]
		writeFakeJSON(w, comment)
		return
	}
	http.NotFound(w, r)
}

// addComment seeds a comment on pull request 77 under author. One under
// Goobers' own login is stored as a daemon run wrote it, with the
// attribution footer.
func (f *fakeGiteaComments) addComment(author, body string) {
	if author == f.login {
		body = stampOwnFixtureBody(body, "comment")
	}
	f.addRawComment(author, body)
}

// addRawComment seeds body under author exactly as given.
func (f *fakeGiteaComments) addRawComment(author, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.comments = append(f.comments, map[string]any{"id": f.nextID, "body": body, "user": map[string]string{"login": author}})
}

func (f *fakeGiteaComments) bodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	bodies := make([]string, 0, len(f.comments))
	for _, comment := range f.comments {
		bodies = append(bodies, comment["body"].(string))
	}
	return bodies
}

// requireSingleAttributedComment checks the end state: exactly one comment
// holding want (ignoring the attribution footer) and carrying task's
// attribution.
func requireSingleAttributedComment(t *testing.T, bodies []string, want, task string) {
	t.Helper()
	if len(bodies) != 1 {
		t.Fatalf("comments = %q, want exactly one", bodies)
	}
	if want != "" && providers.StripAttribution(bodies[0]) != strings.TrimSpace(want) {
		t.Errorf("comment text = %q, want %q", providers.StripAttribution(bodies[0]), strings.TrimSpace(want))
	}
	if attribution, ok, err := providers.ParseAttribution(bodies[0]); err != nil || !ok || attribution.Task != task {
		t.Errorf("comment attribution = %+v, %v, %v; want task %q", attribution, ok, err, task)
	}
}

// TestMergeReviewStatusCommentAttributedWriteCount: with attribution on, the
// merge-review status comment is created with one POST and no edit, and a
// later or retried reconcile edits it exactly once.
func TestMergeReviewStatusCommentAttributedWriteCount(t *testing.T) {
	first := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "first cycle"})
	second := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "later cycle"})
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	type fixture struct {
		provider remediationProvider
		writes   *providerWriteCounter
		bodies   func() []string
		create   string
		edit     string
	}
	fixtures := map[string]func(t *testing.T) fixture{
		"github": func(t *testing.T) fixture {
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(77, "sticky status")
			counter := &providerWriteCounter{}
			provider := server.newGitHubProvider("token", providers.WithHTTPClient(countingHTTPClient{counter: counter, inner: &http.Client{}}))
			provider.SetAttribution(testRunAttribution("apply-verdict"))
			return fixture{
				provider: provider, writes: counter,
				bodies: func() []string { comments, _ := fakeIssueComments(t, server, 77); return comments },
				create: "POST /repos/your-org/your-repo/issues/{n}/comments",
				edit:   "PATCH /repos/your-org/your-repo/issues/comments/{n}",
			}
		},
		"gitea": func(t *testing.T) fixture {
			fake, server := newFakeGiteaComments(t, "goobers")
			provider := providers.NewGiteaProvider(server.URL, "token")
			provider.SetAttribution(testRunAttribution("apply-verdict"))
			return fixture{
				provider: provider, writes: &fake.writes, bodies: fake.bodies,
				create: "POST /api/v1/repos/your-org/your-repo/issues/{n}/comments",
				edit:   "PATCH /api/v1/repos/your-org/your-repo/issues/comments/{n}",
			}
		},
	}
	for name, build := range fixtures {
		t.Run(name, func(t *testing.T) {
			f := build(t)
			steps := []struct {
				label string
				body  string
				want  map[string]int
			}{
				{"create", first, map[string]int{f.create: 1}},
				{"update", second, map[string]int{f.edit: 1}},
				{"retry", second, map[string]int{f.edit: 1}},
			}
			for _, step := range steps {
				if err := reconcileMergeReviewStatusComment(context.Background(), f.provider, repo, 77, step.body); err != nil {
					t.Fatalf("%s reconcile: %v", step.label, err)
				}
				assertProviderWrites(t, step.label, f.writes.take(), step.want)
			}
			requireSingleAttributedComment(t, f.bodies(), second, "apply-verdict")
		})
	}
}

// TestRespondToFindingsAttributedWriteCountOnGitHub: with daemon attribution
// on, respond-to-findings posts its account once and a retry edits it once.
func TestRespondToFindingsAttributedWriteCountOnGitHub(t *testing.T) {
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "validate empty input"}},
	}
	root, server, _ := respondToFindingsFixture(t, verdict,
		`[{"finding":1,"disposition":"addressed","detail":"Added an explicit empty-input guard."}]`, true)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv(executor.TaskEnvVar, "respond-to-findings")
	counter := &providerWriteCounter{}
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return server.newGitHubProvider(token, append(opts, providers.WithHTTPClient(countingHTTPClient{counter: counter, inner: &http.Client{}}))...)
	}

	const (
		post  = "POST /repos/your-org/your-repo/issues/{n}/comments"
		patch = "PATCH /repos/your-org/your-repo/issues/comments/{n}"
	)
	for attempt, want := range []map[string]int{{post: 1}, {patch: 1}} {
		if code, stdout, stderr := runArgs(t, "respond-to-findings", root); code != 0 {
			t.Fatalf("attempt %d: code = %d, stdout = %q, stderr = %q", attempt+1, code, stdout, stderr)
		}
		assertProviderWrites(t, fmt.Sprintf("attempt %d", attempt+1), counter.take(), want)
	}
	comments, _ := fakeIssueComments(t, server, 77)
	requireSingleAttributedComment(t, comments, "", "respond-to-findings")
}

// TestRespondToFindingsAttributedWriteCountOnGitea is the Gitea counterpart.
func TestRespondToFindingsAttributedWriteCountOnGitea(t *testing.T) {
	t.Chdir(t.TempDir())
	const runID = "run-gitea-attributed-response"
	fake, server := newFakeGiteaComments(t, "remediation-bot")
	root := initDemo(t)
	configureRemediationGitea(t, root, server.URL)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv(executor.TaskEnvVar, "respond-to-findings")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitea))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "gitea-issues-token")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), remediationResponseArtifactName))
	if _, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	seedRemediationResponseRun(t, root, runID, apiv1.Verdict{}, "", true)

	const (
		post  = "POST /api/v1/repos/your-org/your-repo/issues/{n}/comments"
		patch = "PATCH /api/v1/repos/your-org/your-repo/issues/comments/{n}"
	)
	for attempt, want := range []map[string]int{{post: 1}, {patch: 1}} {
		if code, stdout, stderr := runArgs(t, "respond-to-findings", root); code != 0 {
			t.Fatalf("attempt %d: code = %d, stdout = %q, stderr = %q", attempt+1, code, stdout, stderr)
		}
		assertProviderWrites(t, fmt.Sprintf("attempt %d", attempt+1), fake.writes.take(), want)
	}
	bodies := fake.bodies()
	requireSingleAttributedComment(t, bodies, "", "respond-to-findings")
	if !strings.HasPrefix(bodies[0], remediationResponseMarker(runID)) {
		t.Errorf("comment = %q, want the run-scoped remediation response", bodies[0])
	}
}
