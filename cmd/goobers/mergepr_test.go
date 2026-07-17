package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// mergePRServerState scripts one PR's live state for #360's conjunctive
// merge-action tests — a purpose-built mux (rather than the shared
// fakeGitHubServer, which is scoped to backlog-query/open-pr/issue-close-out
// and has no /merge endpoint) so every conjunct (CI state, draft, head/base
// SHA) is independently controllable per test case.
type mergePRServerState struct {
	draft           bool
	checkState      string // "success" or "failure" — maps to CheckStatePassing/Failing
	headSHA         string
	headBranch      string
	headOwner       string
	headRepo        string
	baseSHA         string
	stacked         bool
	pullListStatus  int
	deleteStatus    int
	mergeCalls      int
	pullListCalls   int
	deleteCalls     int
	baseListCalls   int
	baseDeleteCalls int
	mergeSHA        *string // set by the /merge handler on a successful call
	mergeBody       map[string]interface{}
	commentCalls    int
	// verdictOnSecondCommentPage forces the pass verdict onto page 2 of the
	// comments endpoint, behind 100 routine comments and a Link: rel="next"
	// header — proves structuredMergeCommitMessage's verdict lookup follows
	// pagination rather than only checking the first page.
	verdictOnSecondCommentPage bool
	// files is this PR's own changed files (issue #718's delta-aware
	// baseSha conjunct: what base's movement is checked for intersecting).
	// baseMovement maps a "oldBaseSHA...newBaseSHA" compare key to the
	// files that moved between them — an unregistered key returns an
	// empty file list (a disjoint move, the common steady-state case), so
	// most test cases need no entry at all.
	files        []fakePRFile
	baseMovement map[string][]fakePRFile
}

func newMergePRServer(t *testing.T, owner, repo string, st *mergePRServerState) *httptest.Server {
	t.Helper()
	if st.headBranch == "" {
		st.headBranch = "goobers/implementation/run-1"
	}
	if st.headOwner == "" {
		st.headOwner = owner
	}
	if st.headRepo == "" {
		st.headRepo = repo
	}
	prefix := "/repos/" + owner + "/" + repo
	headPrefix := "/repos/" + st.headOwner + "/" + st.headRepo
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{
			"number": 9, "state": "open", "merged": false, "draft": st.draft,
			"html_url": "https://github.com/" + owner + "/" + repo + "/pull/9",
			"title":    "Implement structured merge messages",
			"body":     "Implements the requested behavior.\n\nFixes #42",
			"head": map[string]interface{}{
				"ref": st.headBranch, "sha": st.headSHA,
				"repo": map[string]interface{}{
					"name": st.headRepo, "html_url": "https://github.com/" + st.headOwner + "/" + st.headRepo,
					"owner": map[string]string{"login": st.headOwner},
				},
			},
			"base": map[string]interface{}{"sha": st.baseSHA},
		})
	})
	mux.HandleFunc(headPrefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		st.pullListCalls++
		if st.pullListStatus != 0 {
			http.Error(w, "list failed", st.pullListStatus)
			return
		}
		if got := r.URL.Query().Get("base"); got != st.headBranch {
			t.Errorf("pull-list base = %q, want %q", got, st.headBranch)
		}
		if !st.stacked {
			writeFakeJSON(w, []map[string]interface{}{})
			return
		}
		writeFakeJSON(w, []map[string]interface{}{{
			"number": 10, "state": "open", "html_url": "https://github.com/" + owner + "/" + repo + "/pull/10",
			"head": map[string]interface{}{"ref": "goobers/stacked/child", "sha": "child-sha"},
			"base": map[string]interface{}{"ref": st.headBranch, "sha": st.headSHA},
		}})
	})
	if headPrefix != prefix {
		mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
			st.baseListCalls++
			writeFakeJSON(w, []map[string]interface{}{})
		})
	}
	mux.HandleFunc(prefix+"/pulls/9/reviews", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, []map[string]interface{}{})
	})
	mux.HandleFunc(fmt.Sprintf("%s/commits/%s/status", prefix, st.headSHA), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"state": st.checkState, "statuses": []map[string]interface{}{}})
	})
	mux.HandleFunc(fmt.Sprintf("%s/commits/%s/check-runs", prefix, st.headSHA), func(w http.ResponseWriter, r *http.Request) {
		// combinedCheckState treats zero check details as CheckStatePending
		// regardless of checkState (github.go: "pending || len(details) == 0")
		// — a real "success" PR always has at least one check-run, so the fake
		// server must too, or "success" here would silently mean "pending".
		conclusion := "success"
		if st.checkState == "failure" {
			conclusion = "failure"
		}
		writeFakeJSON(w, map[string]interface{}{"check_runs": []map[string]interface{}{
			{"name": "make-ci", "status": "completed", "conclusion": conclusion, "html_url": "https://ci/make-ci"},
		}})
	})
	mux.HandleFunc(prefix+"/issues/9/comments", func(w http.ResponseWriter, r *http.Request) {
		st.commentCalls++
		if st.verdictOnSecondCommentPage && r.URL.Query().Get("page") != "2" {
			comments := make([]map[string]interface{}, 100)
			for i := range comments {
				comments[i] = map[string]interface{}{"id": i + 1, "body": "Routine pull request comment."}
			}
			w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=2>; rel=\"next\"", r.Host, r.URL.Path))
			writeFakeJSON(w, comments)
			return
		}
		comment := renderVerdictComment(apiv1.Verdict{
			Decision:  apiv1.VerdictPass,
			Summary:   "The implementation is ready to merge.",
			Rationale: "It satisfies the issue while preserving the existing merge safety checks.",
			HeadSHA:   st.headSHA,
			BaseSHA:   st.baseSHA,
		})
		writeFakeJSON(w, []map[string]interface{}{{"id": 1, "body": comment}})
	})
	mux.HandleFunc(prefix+"/pulls/9/files", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]interface{}, 0, len(st.files))
		for _, f := range st.files {
			out = append(out, map[string]interface{}{"filename": f.path, "status": f.status, "additions": f.additions, "deletions": f.deletions, "patch": f.patch})
		}
		writeFakeJSON(w, out)
	})
	mux.HandleFunc(prefix+"/compare/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, prefix+"/compare/")
		files := st.baseMovement[key]
		out := make([]map[string]interface{}, 0, len(files))
		for _, f := range files {
			out = append(out, map[string]interface{}{"filename": f.path, "status": f.status, "additions": f.additions, "deletions": f.deletions, "patch": f.patch})
		}
		writeFakeJSON(w, map[string]interface{}{"merge_base_commit": map[string]interface{}{"sha": "irrelevant-for-this-fixture"}, "files": out})
	})
	mux.HandleFunc(prefix+"/pulls/9/merge", func(w http.ResponseWriter, r *http.Request) {
		st.mergeCalls++
		if err := json.NewDecoder(r.Body).Decode(&st.mergeBody); err != nil {
			t.Errorf("decode merge request body: %v", err)
		}
		sha := "merge-commit-sha"
		st.mergeSHA = &sha
		writeFakeJSON(w, map[string]interface{}{"sha": sha, "merged": true, "message": "merged"})
	})
	mux.HandleFunc(headPrefix+"/git/refs/heads/"+st.headBranch, func(w http.ResponseWriter, r *http.Request) {
		st.deleteCalls++
		if r.Method != http.MethodDelete {
			t.Errorf("branch request method = %s, want DELETE", r.Method)
		}
		if st.deleteStatus != 0 {
			http.Error(w, "delete failed", st.deleteStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if headPrefix != prefix {
		mux.HandleFunc(prefix+"/git/refs/heads/"+st.headBranch, func(w http.ResponseWriter, r *http.Request) {
			st.baseDeleteCalls++
			w.WriteHeader(http.StatusNoContent)
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// mergePRTestServer wraps an httptest.Server so it satisfies the
// server.newGitHubProvider(token, opts...) shape providerCmdEnv-style tests
// use, pointing every constructed provider's BaseURL at it.
type mergePRTestServer struct{ url string }

func (s mergePRTestServer) newGitHubProvider(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
	return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = s.url })...)
}

// mergePREnv sets up a runnable merge-pr CLI invocation: instance root,
// run/workflow identity, the github:pr:merge credential (unless
// withoutCapability), and declared Task.Inputs as GOOBERS_INPUT_* env vars.
// Returns (instanceRoot, workDir): instanceRoot is the explicit [path] arg
// runArgs must be called with; workDir is where the result file lands (cwd,
// since resultFile defaults to a bare relative filename) — mirroring
// TestOpenPRCreatesThenUpdatesOnRepass's split between the two.
func mergePREnv(t *testing.T, serverURL string, withoutCapability bool, inputs map[string]string) (instanceRoot, workDir string) {
	t.Helper()
	instanceRoot = initDemo(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: serverURL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-merge-1")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	if !withoutCapability {
		t.Setenv("GOOBERS_CRED_GITHUB_PR_MERGE", "test-token")
	}
	t.Setenv("GOOBERS_CRED_GITHUB_BRANCH_DELETE", "test-token")
	for k, v := range inputs {
		t.Setenv("GOOBERS_INPUT_"+strings.ToUpper(k), v)
	}
	workDir = t.TempDir()
	t.Chdir(workDir)
	return instanceRoot, workDir
}

func readMergeResult(t *testing.T, dir string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "merge-result.json"))
	if err != nil {
		t.Fatalf("read merge-result.json: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal merge-result.json: %v", err)
	}
	return result
}

// TestMergePRAllConjunctsMetMerges is #360's headline acceptance: verdict
// pass + CI green + not-draft + a matching SHA-pin, capability granted -> the
// PR is actually merged.
func TestMergePRAllConjunctsMetMerges(t *testing.T) {
	st := &mergePRServerState{draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if st.mergeCalls != 1 {
		t.Fatalf("merge endpoint called %d times, want 1", st.mergeCalls)
	}
	if st.pullListCalls != 1 || st.deleteCalls != 1 {
		t.Fatalf("cleanup calls = list:%d delete:%d, want 1 each", st.pullListCalls, st.deleteCalls)
	}
	result := readMergeResult(t, dir)
	if merged, _ := result["merged"].(bool); !merged {
		t.Fatalf("result = %+v, want merged=true", result)
	}
	if result["mergeSha"] != "merge-commit-sha" {
		t.Fatalf("result = %+v, want mergeSha set", result)
	}
	if st.mergeBody["merge_method"] != "squash" {
		t.Fatalf("merge_method = %v, want squash", st.mergeBody["merge_method"])
	}
	if st.mergeBody["commit_title"] != "Implement structured merge messages" {
		t.Fatalf("commit_title = %q, want PR title", st.mergeBody["commit_title"])
	}
	wantMessage := "The implementation is ready to merge.\n\n" +
		"It satisfies the issue while preserving the existing merge safety checks.\n\n" +
		"Closes #42"
	if st.mergeBody["commit_message"] != wantMessage {
		t.Fatalf("commit_message = %q, want %q", st.mergeBody["commit_message"], wantMessage)
	}
	if result["selectedNumber"] != "9" {
		t.Fatalf("result = %+v, want selectedNumber=%q", result, "9")
	}
	if result["branchCleanup"] != "deleted" || result["headBranch"] != st.headBranch {
		t.Fatalf("result = %+v, want deleted branch cleanup for %q", result, st.headBranch)
	}
	facts := readMutationFacts(t, dir)
	if len(facts) != 2 || facts[0].Operation != "merge" || facts[1].Kind != "branch" || facts[1].Operation != "delete" {
		t.Fatalf("mutation facts = %+v, want merge followed by branch delete", facts)
	}
}

func TestMergePRDeletesForkHeadBranchInForkRepository(t *testing.T) {
	st := &mergePRServerState{
		draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456",
		headOwner: "contributor", headRepo: "your-repo-fork",
	}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if st.pullListCalls != 1 || st.deleteCalls != 1 {
		t.Fatalf("fork cleanup calls = list:%d delete:%d, want 1 each", st.pullListCalls, st.deleteCalls)
	}
	if st.baseListCalls != 0 || st.baseDeleteCalls != 0 {
		t.Fatalf("base repository cleanup calls = list:%d delete:%d, want 0", st.baseListCalls, st.baseDeleteCalls)
	}
	result := readMergeResult(t, dir)
	if result["merged"] != true || result["branchCleanup"] != "deleted" {
		t.Fatalf("result = %+v, want merged with deleted fork branch", result)
	}
	facts := readMutationFacts(t, dir)
	if len(facts) != 2 || facts[1].Kind != "branch" || facts[1].ID != st.headBranch || facts[1].Operation != "delete" {
		t.Fatalf("mutation facts = %+v, want fork branch deletion", facts)
	}
}

func TestMergePRKeepsStackedHeadBranch(t *testing.T) {
	st := &mergePRServerState{
		draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456", stacked: true,
	}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if st.deleteCalls != 0 {
		t.Fatalf("delete endpoint called %d times, want 0 for stacked branch", st.deleteCalls)
	}
	result := readMergeResult(t, dir)
	if result["merged"] != true || result["branchCleanup"] != "skipped-stacked" {
		t.Fatalf("result = %+v, want merged with stacked cleanup skip", result)
	}
	facts := readMutationFacts(t, dir)
	if len(facts) != 1 || facts[0].Operation != "merge" {
		t.Fatalf("mutation facts = %+v, want no branch mutation for guarded skip", facts)
	}
}

func TestMergePRDeleteFailurePreservesMergeResult(t *testing.T) {
	st := &mergePRServerState{
		draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456", deleteStatus: http.StatusUnprocessableEntity,
	}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readMergeResult(t, dir)
	if result["merged"] != true || result["mergeSha"] != "merge-commit-sha" || result["branchCleanup"] != "failed" {
		t.Fatalf("result = %+v, want successful merge with failed cleanup", result)
	}
	if result["branchCleanupError"] == "" || !strings.Contains(stderr, "branch cleanup failed") {
		t.Fatalf("cleanup failure not visible: result=%+v stderr=%q", result, stderr)
	}
	facts := readMutationFacts(t, dir)
	if len(facts) != 1 || facts[0].Operation != "merge" {
		t.Fatalf("mutation facts = %+v, want no branch mutation for failed delete", facts)
	}
}

func TestMergePRGuardFailurePreservesMergeResult(t *testing.T) {
	st := &mergePRServerState{
		draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456", pullListStatus: http.StatusUnprocessableEntity,
	}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readMergeResult(t, dir)
	if result["merged"] != true || result["branchCleanup"] != "failed" {
		t.Fatalf("result = %+v, want successful merge with failed cleanup guard", result)
	}
	if !strings.Contains(result["branchCleanupError"].(string), "check stacked pull requests") {
		t.Fatalf("result = %+v, want visible guard provider failure", result)
	}
	if st.deleteCalls != 0 {
		t.Fatalf("delete endpoint called %d times after guard failure, want 0", st.deleteCalls)
	}
}

func TestMergePRDeleteRequiresCapabilityWithoutRewritingMerge(t *testing.T) {
	st := &mergePRServerState{draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})
	t.Setenv("GOOBERS_CRED_GITHUB_BRANCH_DELETE", "")

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readMergeResult(t, dir)
	if result["merged"] != true || result["branchCleanup"] != "failed" {
		t.Fatalf("result = %+v, want successful merge with capability-gated cleanup", result)
	}
	if !strings.Contains(result["branchCleanupError"].(string), "GOOBERS_CRED_GITHUB_BRANCH_DELETE") {
		t.Fatalf("result = %+v, want missing branch-delete capability to be visible", result)
	}
	if st.deleteCalls != 0 {
		t.Fatalf("delete endpoint called %d times without capability, want 0", st.deleteCalls)
	}
}

func TestMergePRUsesConfiguredMergeMethod(t *testing.T) {
	st := &mergePRServerState{draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, _ := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
		"mergeMethod": "rebase",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if st.mergeBody["merge_method"] != "rebase" {
		t.Fatalf("merge_method = %v, want rebase", st.mergeBody["merge_method"])
	}
}

func TestMergePRFindsVerdictBeyondFirstCommentPage(t *testing.T) {
	st := &mergePRServerState{
		draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456",
		verdictOnSecondCommentPage: true,
	}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, _ := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if st.commentCalls != 2 {
		t.Fatalf("comments endpoint called %d times, want 2", st.commentCalls)
	}
	if st.mergeCalls != 1 {
		t.Fatalf("merge endpoint called %d times, want 1", st.mergeCalls)
	}
}

func TestMergePRRejectsUnknownMergeMethod(t *testing.T) {
	st := &mergePRServerState{draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, _ := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
		"mergeMethod": "octopus",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	if st.mergeCalls != 0 {
		t.Fatalf("merge endpoint called %d times, want 0", st.mergeCalls)
	}
}

// TestMergePRRefusesOnUnmetConjunct proves a PR missing any ONE conjunct is
// not merged — the acceptance criterion's core claim, exercised across each
// independent conjunct.
func TestMergePRRefusesOnUnmetConjunct(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(st *mergePRServerState, inputs map[string]string)
		wantSub string
	}{
		{
			name:    "needs-changes verdict",
			mutate:  func(st *mergePRServerState, inputs map[string]string) { inputs["verdict"] = "needs-changes" },
			wantSub: "verdict is",
		},
		{
			name:    "failing CI",
			mutate:  func(st *mergePRServerState, inputs map[string]string) { st.checkState = "failure" },
			wantSub: "CI is",
		},
		{
			name:    "draft PR",
			mutate:  func(st *mergePRServerState, inputs map[string]string) { st.draft = true },
			wantSub: "draft",
		},
		{
			name:    "stale head SHA-pin",
			mutate:  func(st *mergePRServerState, inputs map[string]string) { inputs["headSha"] = "stale-head" },
			wantSub: "verdict is stale",
		},
		{
			name: "stale base SHA-pin, movement intersects this PR's files",
			mutate: func(st *mergePRServerState, inputs map[string]string) {
				inputs["baseSha"] = "stale-base"
				st.files = []fakePRFile{{path: "shared/conflict.go", status: "modified"}}
				st.baseMovement = map[string][]fakePRFile{
					"stale-base...base456": {{path: "shared/conflict.go", status: "modified"}},
				}
			},
			wantSub: "verdict is stale",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &mergePRServerState{draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456"}
			inputs := map[string]string{"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456"}
			tc.mutate(st, inputs)
			server := newMergePRServer(t, "your-org", "your-repo", st)
			root, dir := mergePREnv(t, server.URL, false, inputs)

			code, _, stderr := runArgs(t, "merge-pr", root)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q (a missing conjunct is a normal outcome, not a stage failure)", code, stderr)
			}
			if st.mergeCalls != 0 {
				t.Fatalf("merge endpoint called %d times, want 0 — must never merge with an unmet conjunct", st.mergeCalls)
			}
			result := readMergeResult(t, dir)
			if merged, _ := result["merged"].(bool); merged {
				t.Fatalf("result = %+v, want merged=false", result)
			}
			reason, _ := result["reason"].(string)
			if !strings.Contains(reason, tc.wantSub) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.wantSub)
			}
			if result["selectedNumber"] != "9" {
				t.Fatalf("result = %+v, want selectedNumber=%q", result, "9")
			}
		})
	}
}

// TestMergePRAcceptsDisjointBaseMovement is issue #718's headline
// acceptance for merge-pr's delta-aware SHA-pin: base advancing since
// review (a bare raw-SHA mismatch) does NOT void an otherwise-valid
// verdict when nothing that moved touches this PR's own files — the
// dominant steady-state case, since every OTHER PR merging moves base for
// everyone. Deliberately registers NO st.baseMovement entry for the
// stale-base...base456 pair: an unregistered compare key returns an empty
// file list, i.e. a genuinely disjoint move.
func TestMergePRAcceptsDisjointBaseMovement(t *testing.T) {
	st := &mergePRServerState{
		draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456",
		files: []fakePRFile{{path: "this-pr/own-file.go", status: "modified"}},
	}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "stale-base",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if st.mergeCalls != 1 {
		t.Fatalf("merge endpoint called %d times, want 1 — a disjoint base advance must not block an otherwise-valid merge", st.mergeCalls)
	}
	result := readMergeResult(t, dir)
	if merged, _ := result["merged"].(bool); !merged {
		t.Fatalf("result = %+v, want merged=true (base moved, but disjointly from this PR's own files)", result)
	}
}

func TestMergePRRetriesAfterLiveEligibilityRecovers(t *testing.T) {
	cases := []struct {
		name    string
		block   func(*mergePRServerState)
		recover func(*mergePRServerState)
	}{
		{
			name:    "CI fails after review",
			block:   func(st *mergePRServerState) { st.checkState = "failure" },
			recover: func(st *mergePRServerState) { st.checkState = "success" },
		},
		{
			name:    "PR becomes draft after review",
			block:   func(st *mergePRServerState) { st.draft = true },
			recover: func(st *mergePRServerState) { st.draft = false },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &mergePRServerState{checkState: "success", headSHA: "head123", baseSHA: "base456"}
			tc.block(st)
			server := newMergePRServer(t, "your-org", "your-repo", st)
			root, dir := mergePREnv(t, server.URL, false, map[string]string{
				"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
			})

			code, _, stderr := runArgs(t, "merge-pr", root)
			if code != 0 {
				t.Fatalf("refusal code = %d, stderr = %q", code, stderr)
			}
			if merged, _ := readMergeResult(t, dir)["merged"].(bool); merged {
				t.Fatal("PR merged while its live eligibility differed from the reviewed state")
			}

			tc.recover(st)
			code, _, stderr = runArgs(t, "merge-pr", root)
			if code != 0 {
				t.Fatalf("retry code = %d, stderr = %q", code, stderr)
			}
			if merged, _ := readMergeResult(t, dir)["merged"].(bool); !merged {
				t.Fatal("PR was not merged after its live eligibility recovered")
			}
			if st.mergeCalls != 1 {
				t.Fatalf("merge endpoint called %d times, want 1 after recovery", st.mergeCalls)
			}
		})
	}
}

// TestMergePRAdvisoryModeNeverMerges proves the advisory-mode toggle refuses
// to merge even when every other conjunct holds.
func TestMergePRAdvisoryModeNeverMerges(t *testing.T) {
	st := &mergePRServerState{draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456", "advisoryMode": "true",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if st.mergeCalls != 0 {
		t.Fatalf("merge endpoint called %d times, want 0 in advisory mode", st.mergeCalls)
	}
	result := readMergeResult(t, dir)
	if merged, _ := result["merged"].(bool); merged {
		t.Fatalf("result = %+v, want merged=false in advisory mode", result)
	}
	if reason, _ := result["reason"].(string); !strings.Contains(reason, "advisory") {
		t.Fatalf("reason = %q, want it to mention advisory mode", reason)
	}
}

// TestMergePRRefusesWithoutCapability is #360's "capability absent ->
// refused" acceptance criterion: no github:pr:merge credential means the
// stage never even reaches the provider (no HTTP call at all), exiting 1.
func TestMergePRRefusesWithoutCapability(t *testing.T) {
	st := &mergePRServerState{draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, true, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (capability absent -> refused), stderr = %q", code, stderr)
	}
	if st.mergeCalls != 0 {
		t.Fatalf("merge endpoint called %d times, want 0 — never even attempted without the capability", st.mergeCalls)
	}
	if _, err := os.Stat(filepath.Join(dir, "merge-result.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no result file to be written when refused for missing capability")
	}
}

// TestMergePRWaitsForHeldMergeLock is issue #719's core acceptance: with
// merge-review's readiness now allowing several concurrent runs to review
// DIFFERENT PRs at once, only one PR may be inside merge-pr's poll->decide->
// merge window at a time. Simulates a concurrent run already holding that
// window (an external flock on the exact same instance-scoped lock file,
// released only after a delay) and asserts this invocation's poll — and
// therefore its whole decision, including the actual merge call — does not
// happen until the held lock is released. A merge-pr that raced ahead
// unlocked would return well before the release and could poll/decide
// against state an in-flight sibling merge was about to change out from
// under it (the exact TOCTOU the serialization exists to close).
func TestMergePRWaitsForHeldMergeLock(t *testing.T) {
	st := &mergePRServerState{draft: false, checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})

	l := layoutFor(root)
	lockPath := filepath.Join(l.SchedulerDir(), mergeLockFileName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("pre-acquire merge lock: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}
	const holdFor = 150 * time.Millisecond
	released := make(chan time.Time, 1)
	go func() {
		time.Sleep(holdFor)
		released <- time.Now()
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}()

	code, _, stderr := runArgs(t, "merge-pr", root)
	returnedAt := time.Now()
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	releasedAt := <-released
	if returnedAt.Before(releasedAt) {
		t.Fatalf("merge-pr returned at %v, before the held lock released at %v — its poll/decide/merge window was not actually gated by the lock", returnedAt, releasedAt)
	}
	if st.mergeCalls != 1 {
		t.Fatalf("merge endpoint called %d times, want 1 (merged only once the lock freed)", st.mergeCalls)
	}
	result := readMergeResult(t, dir)
	if merged, _ := result["merged"].(bool); !merged {
		t.Fatalf("result = %+v, want merged=true once the lock freed and every conjunct still held", result)
	}
}

// TestMergePRReleasesLockOnRefusal proves the lock is released even when the
// stage refuses to merge (an unmet conjunct) — a leaked lock on the refusal
// path would starve every subsequent merge-review run, turning a routine
// "not ready yet" outcome into a wedged instance.
func TestMergePRReleasesLockOnRefusal(t *testing.T) {
	st := &mergePRServerState{draft: true, checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readMergeResult(t, dir)
	if merged, _ := result["merged"].(bool); merged {
		t.Fatalf("result = %+v, want merged=false (draft PR)", result)
	}

	// A held-but-not-released lock would make this second, independent
	// invocation hang; runArgs is synchronous, so a passing test here (within
	// the normal test timeout) is itself proof the first call released it —
	// still assert explicitly on the outcome, not just "it didn't hang".
	code2, _, stderr2 := runArgs(t, "merge-pr", root)
	if code2 != 0 {
		t.Fatalf("second merge-pr: code = %d, stderr = %q", code2, stderr2)
	}
	if st.mergeCalls != 0 {
		t.Fatalf("merge endpoint called %d times across both runs, want 0 (still a draft)", st.mergeCalls)
	}
}
