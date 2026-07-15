package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// postMergeServerState is a small stateful fake GitHub server purpose-built
// for #361's post-merge fan-out + close-out tests: the merged PR's poll
// response, a set of other open PRs targeting the same base, and the
// referenced issue's label/comment/state history.
type postMergeServerState struct {
	mu sync.Mutex

	mergedNumber int
	baseBranch   string
	body         string

	otherOpenPRs []int // numbers of other open PRs targeting baseBranch

	labeledPRs    []int // PR numbers that received needsRemediationLabel
	issueLabels   map[int][]string
	issueState    map[int]string
	issueComments map[int][]string
}

func newPostMergeServer(t *testing.T, owner, repo string, st *postMergeServerState) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + owner + "/" + repo
	mux := http.NewServeMux()

	// The merged PR's own poll.
	mux.HandleFunc(fmt.Sprintf("%s/pulls/%d", prefix, st.mergedNumber), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{
			"number": st.mergedNumber, "state": "closed", "merged": true,
			"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, st.mergedNumber),
			"body":     st.body,
			"head":     map[string]interface{}{"sha": "mergedsha"},
			"base":     map[string]interface{}{"sha": "basesha", "ref": st.baseBranch},
		})
	})
	mux.HandleFunc(fmt.Sprintf("%s/pulls/%d/reviews", prefix, st.mergedNumber), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, []map[string]interface{}{})
	})
	mux.HandleFunc(fmt.Sprintf("%s/commits/mergedsha/status", prefix), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"state": "success", "statuses": []map[string]interface{}{}})
	})
	mux.HandleFunc(fmt.Sprintf("%s/commits/mergedsha/check-runs", prefix), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"check_runs": []map[string]interface{}{}})
	})
	mux.HandleFunc(fmt.Sprintf("%s/issues/%d/comments", prefix, st.mergedNumber), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, []map[string]interface{}{})
	})

	// The other-open-PRs listing.
	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("base"); got != st.baseBranch {
			t.Fatalf("ListPullRequests base query = %q, want %q", got, st.baseBranch)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		out := []map[string]interface{}{}
		for _, n := range st.otherOpenPRs {
			out = append(out, map[string]interface{}{
				"number": n, "html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n),
				"head": map[string]interface{}{"ref": fmt.Sprintf("goobers/impl/run-%d", n)},
				"base": map[string]interface{}{"ref": st.baseBranch},
			})
		}
		writeFakeJSON(w, out)
	})

	// GetWorkItem + labels for every other open PR (UpdateWorkItem's flow).
	for _, n := range st.otherOpenPRs {
		n := n
		mux.HandleFunc(fmt.Sprintf("%s/issues/%d", prefix, n), func(w http.ResponseWriter, r *http.Request) {
			writeFakeJSON(w, map[string]interface{}{"number": n, "state": "open", "html_url": fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n)})
		})
		mux.HandleFunc(fmt.Sprintf("%s/issues/%d/labels", prefix, n), func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Labels []string `json:"labels"`
			}
			decodeFakeJSON(r, &body)
			st.mu.Lock()
			st.labeledPRs = append(st.labeledPRs, n)
			st.mu.Unlock()
			writeFakeJSON(w, []map[string]string{})
		})
	}

	// GetWorkItem + labels/state/comment for any referenced issue — path
	// registered generically since the test controls exactly which issue
	// numbers appear in st.body.
	mux.HandleFunc(prefix+"/issues/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, prefix+"/issues/")
		parts := strings.Split(rest, "/")
		num, err := strconv.Atoi(parts[0])
		if err != nil {
			http.Error(w, "bad issue number", http.StatusBadRequest)
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case len(parts) == 1 && r.Method == http.MethodGet:
			state := st.issueState[num]
			if state == "" {
				state = "open"
			}
			writeFakeJSON(w, map[string]interface{}{"number": num, "state": state, "labels": labelsJSON(st.issueLabels[num]), "html_url": fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, num)})
		case len(parts) == 1 && r.Method == http.MethodPatch:
			var body struct {
				State string `json:"state"`
			}
			decodeFakeJSON(r, &body)
			if body.State != "" {
				st.issueState[num] = body.State
			}
			writeFakeJSON(w, map[string]interface{}{"number": num, "state": st.issueState[num]})
		case len(parts) == 2 && parts[1] == "labels" && r.Method == http.MethodPost:
			var body struct {
				Labels []string `json:"labels"`
			}
			decodeFakeJSON(r, &body)
			st.issueLabels[num] = append(st.issueLabels[num], body.Labels...)
			writeFakeJSON(w, []map[string]string{})
		case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			decodeFakeJSON(r, &body)
			st.issueComments[num] = append(st.issueComments[num], body.Body)
			writeFakeJSON(w, map[string]interface{}{"id": len(st.issueComments[num])})
		default:
			http.Error(w, fmt.Sprintf("unhandled %s %s", r.Method, r.URL.Path), http.StatusNotImplemented)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func labelsJSON(labels []string) []map[string]string {
	out := make([]map[string]string, len(labels))
	for i, l := range labels {
		out[i] = map[string]string{"name": l}
	}
	return out
}

// postMergeEnv sets up a runnable post-merge CLI invocation, mirroring
// mergePREnv's shape.
func postMergeEnv(t *testing.T, serverURL string, withoutCapability bool, inputs map[string]string) (instanceRoot, workDir string) {
	t.Helper()
	instanceRoot = initDemo(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: serverURL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-postmerge-1")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	if !withoutCapability {
		t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
		t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	}
	for k, v := range inputs {
		t.Setenv("GOOBERS_INPUT_"+strings.ToUpper(k), v)
	}
	workDir = t.TempDir()
	t.Chdir(workDir)
	return instanceRoot, workDir
}

// TestPostMergeFansOutAndClosesReferencedIssue is #361's headline
// acceptance: merging PR #20 (base main, body "Fixes #42") labels the two
// other open PRs targeting main needs-remediation, and closes issue #42 —
// not on PR-open, on the merge event.
func TestPostMergeFansOutAndClosesReferencedIssue(t *testing.T) {
	st := &postMergeServerState{
		mergedNumber: 20, baseBranch: "main", body: "Implements the thing.\n\nFixes #42",
		otherOpenPRs:  []int{21, 22},
		issueLabels:   map[int][]string{},
		issueState:    map[int]string{},
		issueComments: map[int][]string{},
	}
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, stdout, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	st.mu.Lock()
	labeled := append([]int(nil), st.labeledPRs...)
	issueState := st.issueState[42]
	issueComments := append([]string(nil), st.issueComments[42]...)
	st.mu.Unlock()

	if len(labeled) != 2 {
		t.Fatalf("labeled PRs = %v, want exactly [21 22] (order-independent count 2)", labeled)
	}
	for _, want := range []int{21, 22} {
		found := false
		for _, n := range labeled {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("PR #%d was not labeled; labeled = %v", want, labeled)
		}
	}
	if issueState != "closed" {
		t.Fatalf("issue #42 state = %q, want closed", issueState)
	}
	if len(issueComments) != 1 || !strings.Contains(issueComments[0], "#20") {
		t.Fatalf("issue #42 comments = %v, want one mentioning pull request #20", issueComments)
	}
}

// TestPostMergeNoIssueReferenceIsNotAnError proves a merged PR whose body
// references no backlog issue still succeeds — the fan-out still runs, but
// there is simply nothing to close.
func TestPostMergeNoIssueReferenceIsNotAnError(t *testing.T) {
	st := &postMergeServerState{
		mergedNumber: 20, baseBranch: "main", body: "A manual fix, not tied to a backlog issue.",
		otherOpenPRs:  []int{21},
		issueLabels:   map[int][]string{},
		issueState:    map[int]string{},
		issueComments: map[int][]string{},
	}
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, stdout, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "closed 0 issue") {
		t.Fatalf("stdout = %q, want a mention of 0 issues closed", stdout)
	}
	st.mu.Lock()
	labeled := len(st.labeledPRs)
	st.mu.Unlock()
	if labeled != 1 {
		t.Fatalf("labeled PR count = %d, want 1 (fan-out still runs)", labeled)
	}
}

// TestPostMergeRefusesWithoutCapability proves post-merge fails closed
// before any provider call when either required capability is absent.
func TestPostMergeRefusesWithoutCapability(t *testing.T) {
	st := &postMergeServerState{
		mergedNumber: 20, baseBranch: "main", body: "Fixes #42",
		issueLabels: map[int][]string{}, issueState: map[int]string{}, issueComments: map[int][]string{},
	}
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, true, map[string]string{"pullNumber": "20"})

	code, _, stderr := runArgs(t, "post-merge", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	st.mu.Lock()
	closed := st.issueState[42]
	st.mu.Unlock()
	if closed == "closed" {
		t.Fatal("issue #42 was closed despite the missing capability")
	}
}

// TestClosingIssueNumbers exercises the closing-keyword parser directly
// against GitHub's own recognized forms (case-insensitive close/fix/resolve
// + tense variants) and confirms duplicates collapse to one entry.
func TestClosingIssueNumbers(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"fixes", "Implements the thing.\n\nFixes #42", []string{"42"}},
		{"closes-lowercase", "closes #7", []string{"7"}},
		{"resolved-past-tense", "Resolved #100 for good.", []string{"100"}},
		{"no-reference", "Just a manual fix, no backlog issue.", nil},
		{"multiple-distinct", "Fixes #1 and closes #2", []string{"1", "2"}},
		{"duplicate-collapses", "Fixes #5. Also fixes #5 again.", []string{"5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := closingIssueNumbers(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("closingIssueNumbers(%q) = %v, want %v", tc.body, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("closingIssueNumbers(%q) = %v, want %v", tc.body, got, tc.want)
				}
			}
		})
	}
}
