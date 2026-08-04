package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// commentWatchWorkDir chdirs into a fresh temp dir the stage's result file lands
// in and returns it, mirroring backlogquery_test.go's harness.
func commentWatchWorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func readCommentWatchResult(t *testing.T, dir string) prCommentWatchResult {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, prCommentWatchResultName))
	if err != nil {
		t.Fatalf("read %s: %v", prCommentWatchResultName, err)
	}
	var result prCommentWatchResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal %s: %v", prCommentWatchResultName, err)
	}
	return result
}

// TestPRCommentWatchLabelsPRWithUnaddressedHumanComment is the core acceptance:
// a goober PR whose newest human comment is newer than the bot's own newest
// comment gets goobers:needs-remediation.
func TestPRCommentWatchLabelsPRWithUnaddressedHumanComment(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha1", "base1", false, nil, nil)
	server.addIssue(1, "Add feature")
	t1 := time.Now().UTC().Add(-2 * time.Hour)
	t2 := t1.Add(time.Hour)
	server.addCommentAtAs(1, "goobers", "on it", t1)       // the bot's own login
	server.addCommentAtAs(1, "gneitzke", "please fix", t2) // a human, newer

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	dir := commentWatchWorkDir(t)

	code, stdout, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !server.issueHasLabel(1, needsRemediationLabel) {
		t.Fatalf("issue 1 labels = %v, want %s", server.issueLabels(1), needsRemediationLabel)
	}
	result := readCommentWatchResult(t, dir)
	if result.Scanned != 1 || result.Labeled != 1 || result.Errors != 0 {
		t.Fatalf("result = %+v, want scanned 1 labeled 1 errors 0", result)
	}
	if len(result.PRs) != 1 || result.PRs[0].CommentAuthor != "gneitzke" {
		t.Fatalf("result.PRs = %+v, want one entry authored by gneitzke", result.PRs)
	}
	if result.BotLogin != "goobers" {
		t.Fatalf("result.BotLogin = %q, want goobers", result.BotLogin)
	}
}

// TestPRCommentWatchQuietWhenBotCommentIsNewest confirms a bot comment landing
// after the human's (the merge-review/respond-to-findings answer) closes the
// watermark.
func TestPRCommentWatchQuietWhenBotCommentIsNewest(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha1", "base1", false, nil, nil)
	server.addIssue(1, "Add feature")
	base := time.Now().UTC().Add(-3 * time.Hour)
	server.addCommentAtAs(1, "goobers", "on it", base)
	server.addCommentAtAs(1, "gneitzke", "please fix", base.Add(time.Hour))
	server.addCommentAtAs(1, "goobers", "done", base.Add(2*time.Hour))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	dir := commentWatchWorkDir(t)

	code, _, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
	}
	if server.issueHasLabel(1, needsRemediationLabel) {
		t.Fatalf("issue 1 should not be labeled: %v", server.issueLabels(1))
	}
	if result := readCommentWatchResult(t, dir); result.Labeled != 0 {
		t.Fatalf("result = %+v, want labeled 0", result)
	}
}

// TestPRCommentWatchIgnoresBotTypedAndExcludedAuthors confirms third-party
// automation never triggers: a Bot-typed comment (GitHub's AuthorType) and a
// plain-login CI bot named via excludeAuthors are both excluded.
func TestPRCommentWatchIgnoresBotTypedAndExcludedAuthors(t *testing.T) {
	t.Run("bot-typed trailing comment does not suppress the human trigger", func(t *testing.T) {
		root := initDemo(t)
		server := newFakeGitHubServer(t, "your-org", "your-repo")
		server.addOpenPR(1, "goobers/implementation/x", "main", "sha1", "base1", false, nil, nil)
		server.addIssue(1, "Add feature")
		base := time.Now().UTC().Add(-3 * time.Hour)
		server.addCommentAtAs(1, "goobers", "on it", base)
		server.addCommentAtAs(1, "gneitzke", "please fix", base.Add(time.Hour))
		server.addCommentAtAsType(1, "codecov[bot]", "Bot", "coverage report", base.Add(2*time.Hour))

		providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
		dir := commentWatchWorkDir(t)

		code, _, stderr := runArgs(t, "pr-comment-watch", root)
		if code != 0 {
			t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
		}
		if !server.issueHasLabel(1, needsRemediationLabel) {
			t.Fatalf("issue 1 should be labeled despite the trailing Bot comment: %v", server.issueLabels(1))
		}
		if result := readCommentWatchResult(t, dir); result.Labeled != 1 {
			t.Fatalf("result = %+v, want labeled 1", result)
		}
	})

	t.Run("plain-login CI bot excluded via excludeAuthors input", func(t *testing.T) {
		root := initDemo(t)
		server := newFakeGitHubServer(t, "your-org", "your-repo")
		server.addOpenPR(1, "goobers/implementation/x", "main", "sha1", "base1", false, nil, nil)
		server.addIssue(1, "Add feature")
		base := time.Now().UTC().Add(-3 * time.Hour)
		server.addCommentAtAs(1, "goobers", "on it", base)
		server.addCommentAtAs(1, "gneitzke", "please fix", base.Add(time.Hour))
		server.addCommentAtAs(1, "ci-bot", "build passed", base.Add(2*time.Hour)) // no AuthorType

		providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
		t.Setenv("GOOBERS_INPUT_EXCLUDEAUTHORS", "ci-bot")
		dir := commentWatchWorkDir(t)

		code, _, stderr := runArgs(t, "pr-comment-watch", root)
		if code != 0 {
			t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
		}
		if !server.issueHasLabel(1, needsRemediationLabel) {
			t.Fatalf("issue 1 should be labeled with ci-bot excluded: %v", server.issueLabels(1))
		}
		if result := readCommentWatchResult(t, dir); result.Labeled != 1 {
			t.Fatalf("result = %+v, want labeled 1", result)
		}
	})
}

// TestPRCommentWatchSkipsRoutedParkedAndOptedOutPRs confirms every hard-exclude
// label parks a PR out of the scan even with a fresh human comment. Because
// exclusion happens before result.Scanned++, scanned == 0 also proves no
// comment thread was fetched for them. The two human-decision parks
// (needs-human, merge-escalated) are deliberately NOT here — a fresh comment
// un-parks those (TestPRCommentWatchUnparksHumanDecisionParkOnComment).
func TestPRCommentWatchSkipsRoutedParkedAndOptedOutPRs(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	excludeLabels := []string{
		needsRemediationLabel,
		mergeReadyLabel,
		blockedOnSiblingLabel,
		noMergeReviewLabel,
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i, label := range excludeLabels {
		n := i + 1
		server.addOpenPR(n, "goobers/implementation/x", "main", "sha", "base", false, []string{label}, nil)
		server.addIssue(n, "Parked", label)
		server.addCommentAtAs(n, "goobers", "on it", base)
		server.addCommentAtAs(n, "gneitzke", "please fix", base.Add(time.Hour))
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	dir := commentWatchWorkDir(t)

	code, _, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
	}
	result := readCommentWatchResult(t, dir)
	if result.Scanned != 0 || result.Labeled != 0 {
		t.Fatalf("result = %+v, want scanned 0 labeled 0 (all parked)", result)
	}
	for i, label := range excludeLabels {
		if label == needsRemediationLabel {
			continue // seeded with it already; nothing for the watcher to add
		}
		if server.issueHasLabel(i+1, needsRemediationLabel) {
			t.Fatalf("parked issue %d must not gain %s", i+1, needsRemediationLabel)
		}
	}
}

// TestPRCommentWatchUnparksHumanDecisionParkOnComment is the un-park acceptance:
// a PR parked for a human (needs-human or merge-escalated) that gets a fresh
// human comment is routed back to remediation AND has the park label cleared in
// the same run, so the lane can pick it up instead of leaving it deaf.
func TestPRCommentWatchUnparksHumanDecisionParkOnComment(t *testing.T) {
	for _, park := range []string{providers.LabelNeedsHuman, remediationEscalatedLabel} {
		t.Run(park, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addOpenPR(1, "goobers/implementation/x", "main", "sha1", "base1", false, []string{park}, nil)
			server.addIssue(1, "Parked for a human", park)
			base := time.Now().UTC().Add(-2 * time.Hour)
			server.addCommentAtAs(1, "goobers", "escalating for review", base)
			server.addCommentAtAs(1, "gneitzke", "actually, just do X", base.Add(time.Hour)) // human weighs in

			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
			dir := commentWatchWorkDir(t)

			code, _, stderr := runArgs(t, "pr-comment-watch", root)
			if code != 0 {
				t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
			}
			if !server.issueHasLabel(1, needsRemediationLabel) {
				t.Fatalf("un-parked PR must gain %s: %v", needsRemediationLabel, server.issueLabels(1))
			}
			if server.issueHasLabel(1, park) {
				t.Fatalf("un-parked PR must lose %s: %v", park, server.issueLabels(1))
			}
			result := readCommentWatchResult(t, dir)
			if result.Scanned != 1 || result.Labeled != 1 || result.Unparked != 1 {
				t.Fatalf("result = %+v, want scanned 1 labeled 1 unparked 1", result)
			}
			if len(result.PRs) != 1 || !result.PRs[0].Unparked || len(result.PRs[0].ClearedLabels) != 1 || result.PRs[0].ClearedLabels[0] != park {
				t.Fatalf("result.PRs = %+v, want one un-parked entry clearing %s", result.PRs, park)
			}
		})
	}
}

// TestPRCommentWatchLeavesParkedPRAloneWithoutFreshComment confirms un-parking is
// gated on a genuine fresh human comment: a PR parked for a human whose newest
// comment is the bot's own stays parked (label intact, not routed).
func TestPRCommentWatchLeavesParkedPRAloneWithoutFreshComment(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha1", "base1", false, []string{providers.LabelNeedsHuman}, nil)
	server.addIssue(1, "Parked, no new human word", providers.LabelNeedsHuman)
	base := time.Now().UTC().Add(-2 * time.Hour)
	server.addCommentAtAs(1, "gneitzke", "please look", base)                     // human, older
	server.addCommentAtAs(1, "goobers", "parked for review", base.Add(time.Hour)) // bot, newest

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	dir := commentWatchWorkDir(t)

	code, _, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
	}
	if server.issueHasLabel(1, needsRemediationLabel) {
		t.Fatalf("PR without a fresh human comment must not be routed: %v", server.issueLabels(1))
	}
	if !server.issueHasLabel(1, providers.LabelNeedsHuman) {
		t.Fatalf("PR must stay parked: %v", server.issueLabels(1))
	}
	result := readCommentWatchResult(t, dir)
	if result.Scanned != 1 || result.Labeled != 0 || result.Unparked != 0 {
		t.Fatalf("result = %+v, want scanned 1 labeled 0 unparked 0", result)
	}
}

// TestPRCommentWatchSkipsDraftAndForeignHeadPRs confirms a draft goober PR and
// a PR whose head is outside the branch namespace are both ineligible.
func TestPRCommentWatchSkipsDraftAndForeignHeadPRs(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	base := time.Now().UTC().Add(-2 * time.Hour)
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha", "base", true, nil, nil) // draft
	server.addIssue(1, "Draft PR")
	server.addCommentAtAs(1, "gneitzke", "please fix", base.Add(time.Hour))
	server.addOpenPR(2, "human/feature", "main", "sha", "base", false, nil, nil) // foreign head
	server.addIssue(2, "Human PR")
	server.addCommentAtAs(2, "gneitzke", "please fix", base.Add(time.Hour))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	dir := commentWatchWorkDir(t)

	code, _, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
	}
	result := readCommentWatchResult(t, dir)
	if result.Scanned != 0 || result.Labeled != 0 {
		t.Fatalf("result = %+v, want scanned 0 labeled 0", result)
	}
	if server.issueHasLabel(1, needsRemediationLabel) || server.issueHasLabel(2, needsRemediationLabel) {
		t.Fatalf("neither the draft nor the foreign-head PR should be labeled")
	}
}

// TestPRCommentWatchNoCommentsIsQuiet: a fresh goober PR nobody has touched is
// never labeled.
func TestPRCommentWatchNoCommentsIsQuiet(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha", "base", false, nil, nil)
	server.addIssue(1, "Fresh PR")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	dir := commentWatchWorkDir(t)

	code, _, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
	}
	result := readCommentWatchResult(t, dir)
	if result.Scanned != 1 || result.Labeled != 0 {
		t.Fatalf("result = %+v, want scanned 1 labeled 0", result)
	}
}

// TestPRCommentWatchBoundsScan confirms maxPullRequests caps the number of
// eligible PRs a run inspects and marks the result truncated.
func TestPRCommentWatchBoundsScan(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	base := time.Now().UTC().Add(-2 * time.Hour)
	for n := 1; n <= 2; n++ {
		server.addOpenPR(n, "goobers/implementation/x", "main", "sha", "base", false, nil, nil)
		server.addIssue(n, "PR")
		server.addCommentAtAs(n, "goobers", "on it", base)
		server.addCommentAtAs(n, "gneitzke", "please fix", base.Add(time.Hour))
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_MAXPULLREQUESTS", "1")
	dir := commentWatchWorkDir(t)

	code, _, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
	}
	result := readCommentWatchResult(t, dir)
	if result.Scanned != 1 || !result.Truncated {
		t.Fatalf("result = %+v, want scanned 1 truncated true", result)
	}
}

// TestPRCommentWatchContinuesPastPerPRFailure confirms a transient per-PR
// comment-list failure is warned-and-skipped (errors++) while the other PR is
// still labeled, and the run exits 0.
func TestPRCommentWatchContinuesPastPerPRFailure(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	base := time.Now().UTC().Add(-2 * time.Hour)
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha", "base", false, nil, nil)
	server.addIssue(1, "Poisoned PR")
	server.setIssueCommentsFailure(1, http.StatusInternalServerError, "boom")
	server.addOpenPR(2, "goobers/implementation/y", "main", "sha", "base", false, nil, nil)
	server.addIssue(2, "Healthy PR")
	server.addCommentAtAs(2, "goobers", "on it", base)
	server.addCommentAtAs(2, "gneitzke", "please fix", base.Add(time.Hour))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	dir := commentWatchWorkDir(t)

	code, _, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
	}
	result := readCommentWatchResult(t, dir)
	if result.Scanned != 2 || result.Labeled != 1 || result.Errors != 1 {
		t.Fatalf("result = %+v, want scanned 2 labeled 1 errors 1", result)
	}
	if server.issueHasLabel(1, needsRemediationLabel) {
		t.Fatalf("poisoned PR #1 must not be labeled")
	}
	if !server.issueHasLabel(2, needsRemediationLabel) {
		t.Fatalf("healthy PR #2 should still be labeled: %v", server.issueLabels(2))
	}
}

// giteaCommentWatchFake is a minimal stateful Gitea API stand-in for the first
// cmd-level Gitea stage test: exactly the endpoints pr-comment-watch's Gitea
// arm touches (user, pulls list, issue comments, repo labels list/create,
// issue-label POST, issue GET for the UpdateWorkItem before/after reads).
type giteaCommentWatchFake struct {
	mu              sync.Mutex
	nextLabelID     int64
	labels          map[string]int64
	issueLabelNames []string
	appliedLabelIDs []int64
}

func newGiteaCommentWatchFake() *giteaCommentWatchFake {
	return &giteaCommentWatchFake{labels: map[string]int64{}}
}

func (g *giteaCommentWatchFake) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/user" && r.Method == http.MethodGet:
			writeGiteaJSON(t, w, map[string]string{"login": "goobers-bot"})
		case r.URL.Path == "/api/v1/repos/o/r/pulls" && r.Method == http.MethodGet:
			writeGiteaJSON(t, w, []map[string]interface{}{{
				"number": 1, "title": "Add feature", "state": "open",
				"html_url": "https://gitea.test/o/r/pulls/1",
				"head":     map[string]interface{}{"ref": "goobers/implementation/x", "sha": "sha1"},
				"base":     map[string]interface{}{"ref": "main", "sha": "base1"},
				"labels":   []map[string]interface{}{},
			}})
		case r.URL.Path == "/api/v1/repos/o/r/issues/1/comments" && r.Method == http.MethodGet:
			t1 := time.Now().UTC().Add(-2 * time.Hour)
			writeGiteaJSON(t, w, []map[string]interface{}{
				{"id": 1, "body": "on it", "created_at": t1, "user": map[string]string{"login": "goobers-bot"}},
				{"id": 2, "body": "please fix", "created_at": t1.Add(time.Hour), "user": map[string]string{"login": "gneitzke"}},
			})
		case r.URL.Path == "/api/v1/repos/o/r/labels" && r.Method == http.MethodGet:
			out := make([]map[string]interface{}, 0, len(g.labels))
			for name, id := range g.labels {
				out = append(out, map[string]interface{}{"id": id, "name": name})
			}
			writeGiteaJSON(t, w, out)
		case r.URL.Path == "/api/v1/repos/o/r/labels" && r.Method == http.MethodPost:
			var body map[string]string
			decodeGiteaJSON(t, r, &body)
			g.nextLabelID++
			g.labels[body["name"]] = g.nextLabelID
			writeGiteaJSON(t, w, map[string]interface{}{"id": g.nextLabelID, "name": body["name"]})
		case r.URL.Path == "/api/v1/repos/o/r/issues/1/labels" && r.Method == http.MethodPost:
			var body struct {
				Labels []int64 `json:"labels"`
			}
			decodeGiteaJSON(t, r, &body)
			g.appliedLabelIDs = append(g.appliedLabelIDs, body.Labels...)
			for name, id := range g.labels {
				for _, applied := range body.Labels {
					if applied == id {
						g.issueLabelNames = append(g.issueLabelNames, name)
					}
				}
			}
			writeGiteaJSON(t, w, []map[string]interface{}{})
		case r.URL.Path == "/api/v1/repos/o/r/issues/1" && r.Method == http.MethodGet:
			labels := make([]map[string]interface{}, 0, len(g.issueLabelNames))
			for _, name := range g.issueLabelNames {
				labels = append(labels, map[string]interface{}{"id": g.labels[name], "name": name})
			}
			writeGiteaJSON(t, w, map[string]interface{}{
				"id": 1, "number": 1, "title": "Add feature", "state": "open",
				"html_url": "https://gitea.test/o/r/issues/1", "labels": labels,
			})
		default:
			t.Errorf("unexpected gitea request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotImplemented)
		}
	}
}

func writeGiteaJSON(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode gitea response: %v", err)
	}
}

func decodeGiteaJSON(t *testing.T, r *http.Request, out interface{}) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatalf("decode gitea request: %v", err)
	}
}

// TestPRCommentWatchGiteaEndToEnd drives the Gitea dispatch arm through the real
// GiteaProvider against an inline httptest mux (no newGitea seam), routing the
// stage at a gitea repo via instance.yaml + GOOBERS_REPO_PROVIDER=gitea. The
// label is created on the fly and applied by its resolved ID.
func TestPRCommentWatchGiteaEndToEnd(t *testing.T) {
	root := initDemo(t)
	fake := newGiteaCommentWatchFake()
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)

	cfgPath := instance.NewLayout(root).ConfigFile()
	cfg, err := instance.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Repos = []instance.RepoRef{{
		Provider: string(providers.ProviderGitea),
		BaseURL:  server.URL,
		Owner:    "o",
		Name:     "r",
		Token:    instance.TokenRef{Env: "GOOBERS_GITEA_TOKEN"},
	}}
	if err := instance.WriteConfig(cfgPath, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitea))
	t.Setenv(executor.RepoOwnerEnvVar, "o")
	t.Setenv(executor.RepoNameEnvVar, "r")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	dir := commentWatchWorkDir(t)

	code, stdout, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	fake.mu.Lock()
	labelID, created := fake.labels[needsRemediationLabel]
	applied := append([]int64(nil), fake.appliedLabelIDs...)
	fake.mu.Unlock()
	if !created {
		t.Fatalf("expected %s to be created on the gitea repo", needsRemediationLabel)
	}
	if len(applied) != 1 || applied[0] != labelID {
		t.Fatalf("applied label IDs = %v, want [%d]", applied, labelID)
	}
	result := readCommentWatchResult(t, dir)
	if result.Labeled != 1 || result.BotLogin != "goobers-bot" {
		t.Fatalf("result = %+v, want labeled 1 botLogin goobers-bot", result)
	}
}

// TestLatestUnaddressedHumanComment covers the watermark predicate in isolation:
// index-order fallback for nil timestamps, equal-timestamp tiebreak by position,
// the empty list, and case-insensitive own-login matching.
func TestLatestUnaddressedHumanComment(t *testing.T) {
	at := func(d time.Duration) *time.Time {
		v := time.Unix(0, 0).UTC().Add(d)
		return &v
	}
	comment := func(author, authorType string, created *time.Time) providers.Comment {
		return providers.Comment{Author: author, AuthorType: authorType, CreatedAt: created}
	}

	t.Run("human newer than bot triggers", func(t *testing.T) {
		_, fresh := latestUnaddressedHumanComment([]providers.Comment{
			comment("bot", "", at(time.Hour)),
			comment("dev", "", at(2*time.Hour)),
		}, "bot", nil)
		if !fresh {
			t.Fatal("expected trigger")
		}
	})

	t.Run("bot newer than human is quiet", func(t *testing.T) {
		_, fresh := latestUnaddressedHumanComment([]providers.Comment{
			comment("bot", "", at(time.Hour)),
			comment("dev", "", at(2*time.Hour)),
			comment("bot", "", at(3*time.Hour)),
		}, "bot", nil)
		if fresh {
			t.Fatal("expected quiet")
		}
	})

	t.Run("nil timestamps fall back to list position", func(t *testing.T) {
		// Oldest-first: the human at a later index is newer than the earlier bot.
		got, fresh := latestUnaddressedHumanComment([]providers.Comment{
			comment("bot", "", nil),
			comment("dev", "", nil),
		}, "bot", nil)
		if !fresh || got.Author != "dev" {
			t.Fatalf("got %+v fresh=%v, want dev trigger", got, fresh)
		}
	})

	t.Run("equal timestamps break by position", func(t *testing.T) {
		// Same instant: the bot comment at the later index wins, so no trigger.
		_, fresh := latestUnaddressedHumanComment([]providers.Comment{
			comment("dev", "", at(time.Hour)),
			comment("bot", "", at(time.Hour)),
		}, "bot", nil)
		if fresh {
			t.Fatal("expected quiet when the equal-timestamp bot comment is last")
		}
	})

	t.Run("empty list is quiet", func(t *testing.T) {
		if _, fresh := latestUnaddressedHumanComment(nil, "bot", nil); fresh {
			t.Fatal("expected quiet for an empty thread")
		}
	})

	t.Run("own-login match is case-insensitive", func(t *testing.T) {
		// The only human is the token owner under a different case: no signal.
		if _, fresh := latestUnaddressedHumanComment([]providers.Comment{
			comment("Bot-Account", "", at(time.Hour)),
		}, "bot-account", nil); fresh {
			t.Fatal("expected quiet when the sole author is the bot under a different case")
		}
	})
}
