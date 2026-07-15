package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// mergePRServerState scripts one PR's live state for #360's conjunctive
// merge-action tests — a purpose-built mux (rather than the shared
// fakeGitHubServer, which is scoped to backlog-query/open-pr/issue-close-out
// and has no /merge endpoint) so every conjunct (CI state, draft, head/base
// SHA) is independently controllable per test case.
type mergePRServerState struct {
	draft      bool
	checkState string // "success" or "failure" — maps to CheckStatePassing/Failing
	headSHA    string
	baseSHA    string
	mergeCalls int
	mergeSHA   *string // set by the /merge handler on a successful call
}

func newMergePRServer(t *testing.T, owner, repo string, st *mergePRServerState) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + owner + "/" + repo
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{
			"number": 9, "state": "open", "merged": false, "draft": st.draft,
			"html_url": "https://github.com/" + owner + "/" + repo + "/pull/9",
			"head":     map[string]interface{}{"sha": st.headSHA},
			"base":     map[string]interface{}{"sha": st.baseSHA},
		})
	})
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
		writeFakeJSON(w, []map[string]interface{}{})
	})
	mux.HandleFunc(prefix+"/pulls/9/merge", func(w http.ResponseWriter, r *http.Request) {
		st.mergeCalls++
		sha := "merge-commit-sha"
		st.mergeSHA = &sha
		writeFakeJSON(w, map[string]interface{}{"sha": sha, "merged": true, "message": "merged"})
	})
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
	result := readMergeResult(t, dir)
	if merged, _ := result["merged"].(bool); !merged {
		t.Fatalf("result = %+v, want merged=true", result)
	}
	if result["mergeSha"] != "merge-commit-sha" {
		t.Fatalf("result = %+v, want mergeSha set", result)
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
			name:    "stale base SHA-pin",
			mutate:  func(st *mergePRServerState, inputs map[string]string) { inputs["baseSha"] = "stale-base" },
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
