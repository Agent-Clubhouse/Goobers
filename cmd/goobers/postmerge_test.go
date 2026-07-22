package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/providers"
)

// postMergeServerState is a small stateful fake GitHub server purpose-built
// for #361's post-merge fan-out + close-out tests, extended by #715 to
// support per-sibling triage: each other open PR now carries its own
// mergeable state and touched-files list, and the merged PR's own files are
// served too (the fixed side of every overlap check).
type postMergeServerState struct {
	mu sync.Mutex

	mergedNumber int
	baseBranch   string
	body         string
	mergedFiles  []string // the just-merged PR's own touched files
	merged       bool

	otherOpenPRs []int // numbers of other open PRs targeting baseBranch
	// headSHA/mergeable/files are keyed by PR number — every entry in
	// otherOpenPRs must have one (siblingPR/siblingMergeable/siblingFiles
	// set sane defaults: a fresh head SHA, mergeable=true, no files — i.e.
	// "clean" — so a test only needs to override what it cares about).
	headSHA   map[int]string
	mergeable map[int]*bool
	files     map[int][]string

	labeledPRs    []int // PR numbers that received needsRemediationLabel
	issueLabels   map[int][]string
	issueState    map[int]string
	issueComments map[int][]string
	commentIDs    map[int][]int
	nextCommentID int

	// filesRequests/mergeableRequests count GET /pulls/{n}/files and
	// /pulls/{n} (mergeable) hits per PR number — #715's cache-reuse tests
	// assert a warm sibling cache costs zero extra files requests.
	filesRequests     map[int]int
	mergeableRequests map[int]int
	pollRequests      int
	deleteCalls       int
	deleteStatus      int
	labelStatus       int
	commentStatus     int
}

// newPostMergeServerState returns a state with every map initialized and
// every otherOpenPRs entry defaulted to "clean" (fresh head SHA, mergeable,
// no files) — tests override only what they care about via the setter
// helpers below.
func newPostMergeServerState(mergedNumber int, baseBranch, body string, mergedFiles []string, otherOpenPRs []int) *postMergeServerState {
	st := &postMergeServerState{
		mergedNumber: mergedNumber, baseBranch: baseBranch, body: body, mergedFiles: mergedFiles,
		merged:            true,
		otherOpenPRs:      otherOpenPRs,
		headSHA:           map[int]string{},
		mergeable:         map[int]*bool{},
		files:             map[int][]string{},
		issueLabels:       map[int][]string{},
		issueState:        map[int]string{},
		issueComments:     map[int][]string{},
		commentIDs:        map[int][]int{},
		nextCommentID:     1,
		filesRequests:     map[int]int{},
		mergeableRequests: map[int]int{},
	}
	trueVal := true
	for _, n := range otherOpenPRs {
		st.headSHA[n] = fmt.Sprintf("sha%d", n)
		st.mergeable[n] = &trueVal
		st.files[n] = nil
	}
	return st
}

func (st *postMergeServerState) setConflicted(n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	f := false
	st.mergeable[n] = &f
}

func (st *postMergeServerState) setMergeableUnknown(n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.mergeable[n] = nil
}

func (st *postMergeServerState) setFiles(n int, files ...string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.files[n] = files
}

func (st *postMergeServerState) setHeadSHA(n int, sha string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.headSHA[n] = sha
}

func newPostMergeServer(t *testing.T, owner, repo string, st *postMergeServerState) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + owner + "/" + repo
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]string{"login": "goobers"})
	})

	// The merged PR's own poll.
	mux.HandleFunc(fmt.Sprintf("%s/pulls/%d", prefix, st.mergedNumber), func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.pollRequests++
		merged := st.merged
		st.mu.Unlock()
		state := "open"
		if merged {
			state = "closed"
		}
		writeFakeJSON(w, map[string]interface{}{
			"number": st.mergedNumber, "state": state, "merged": merged,
			"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, st.mergedNumber),
			"body":     st.body,
			"head": map[string]interface{}{
				"ref": "goobers/implementation/run-merged", "sha": "mergedsha",
				"repo": map[string]interface{}{"name": repo, "html_url": "https://github.com/" + owner + "/" + repo, "owner": map[string]string{"login": owner}},
			},
			"base": map[string]interface{}{"sha": "basesha", "ref": st.baseBranch},
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
	// The merged PR's own touched files (fanOutNeedsRemediation's fixed
	// overlap side) — served for the merged number only, distinct from the
	// per-sibling files handler below.
	mux.HandleFunc(fmt.Sprintf("%s/pulls/%d/files", prefix, st.mergedNumber), func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]interface{}, 0, len(st.mergedFiles))
		for _, f := range st.mergedFiles {
			out = append(out, map[string]interface{}{"filename": f, "status": "modified", "additions": 1, "deletions": 0})
		}
		writeFakeJSON(w, out)
	})

	// The other-open-PRs listing.
	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		base := r.URL.Query().Get("base")
		if base == "goobers/implementation/run-merged" {
			writeFakeJSON(w, []map[string]interface{}{})
			return
		}
		if base != st.baseBranch {
			t.Fatalf("ListPullRequests base query = %q, want %q", base, st.baseBranch)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		out := []map[string]interface{}{}
		for _, n := range st.otherOpenPRs {
			out = append(out, map[string]interface{}{
				"number": n, "html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n),
				"head":   map[string]interface{}{"ref": fmt.Sprintf("goobers/impl/run-%d", n), "sha": st.headSHA[n]},
				"base":   map[string]interface{}{"ref": st.baseBranch},
				"labels": labelsJSON(st.issueLabels[n]),
			})
		}
		writeFakeJSON(w, out)
	})
	mux.HandleFunc(prefix+"/git/refs/heads/goobers/implementation/run-merged", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.deleteCalls++
		status := st.deleteStatus
		st.mu.Unlock()
		if status != 0 {
			http.Error(w, "delete failed", status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(prefix+"/issues/comments/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, prefix+"/issues/comments/"))
		if err != nil {
			http.Error(w, "bad comment id", http.StatusBadRequest)
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		decodeFakeJSON(r, &body)
		st.mu.Lock()
		defer st.mu.Unlock()
		for number, ids := range st.commentIDs {
			for i, commentID := range ids {
				if commentID == id {
					st.issueComments[number][i] = body.Body
					writeFakeJSON(w, map[string]interface{}{"id": id, "body": body.Body})
					return
				}
			}
		}
		http.Error(w, "comment not found", http.StatusNotFound)
	})

	// Per-sibling detail (mergeable) + files + labels.
	for _, n := range st.otherOpenPRs {
		n := n
		mux.HandleFunc(fmt.Sprintf("%s/pulls/%d", prefix, n), func(w http.ResponseWriter, r *http.Request) {
			st.mu.Lock()
			st.mergeableRequests[n]++
			mergeable := st.mergeable[n]
			st.mu.Unlock()
			writeFakeJSON(w, map[string]interface{}{"number": n, "mergeable": mergeable})
		})
		mux.HandleFunc(fmt.Sprintf("%s/pulls/%d/files", prefix, n), func(w http.ResponseWriter, r *http.Request) {
			st.mu.Lock()
			st.filesRequests[n]++
			files := st.files[n]
			st.mu.Unlock()
			out := make([]map[string]interface{}, 0, len(files))
			for _, f := range files {
				out = append(out, map[string]interface{}{"filename": f, "status": "modified", "additions": 1, "deletions": 0})
			}
			writeFakeJSON(w, out)
		})
		mux.HandleFunc(fmt.Sprintf("%s/issues/%d", prefix, n), func(w http.ResponseWriter, r *http.Request) {
			writeFakeJSON(w, map[string]interface{}{"number": n, "state": "open", "html_url": fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n)})
		})
		mux.HandleFunc(fmt.Sprintf("%s/issues/%d/labels", prefix, n), func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Labels []string `json:"labels"`
			}
			decodeFakeJSON(r, &body)
			st.mu.Lock()
			status := st.labelStatus
			if status != 0 {
				st.mu.Unlock()
				http.Error(w, "label failed", status)
				return
			}
			st.labeledPRs = append(st.labeledPRs, n)
			st.issueLabels[n] = append(st.issueLabels[n], body.Labels...)
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
			if st.commentStatus != 0 {
				http.Error(w, "comment failed", st.commentStatus)
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			decodeFakeJSON(r, &body)
			id := st.nextCommentID
			st.nextCommentID++
			st.issueComments[num] = append(st.issueComments[num], body.Body)
			st.commentIDs[num] = append(st.commentIDs[num], id)
			writeFakeJSON(w, map[string]interface{}{"id": id})
		case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodGet:
			comments := make([]map[string]interface{}, 0, len(st.issueComments[num]))
			for len(st.commentIDs[num]) < len(st.issueComments[num]) {
				st.commentIDs[num] = append(st.commentIDs[num], st.nextCommentID)
				st.nextCommentID++
			}
			for i, comment := range st.issueComments[num] {
				comments = append(comments, map[string]interface{}{
					"id": st.commentIDs[num][i], "body": comment, "user": map[string]string{"login": "goobers"},
				})
			}
			writeFakeJSON(w, comments)
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

func (st *postMergeServerState) labeledSnapshot() []int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]int(nil), st.labeledPRs...)
}

func assertLabeledExactly(t *testing.T, got []int, want ...int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("labeled PRs = %v, want exactly %v", got, want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("PR #%d was not labeled; labeled = %v", w, got)
		}
	}
}

// TestPostMergeFansOutAndClosesReferencedIssue is #361's headline
// acceptance, reworked for #715's triage: merging PR #20 (base main, body
// "Fixes #42") against two siblings — #21 conflicted, #22 file-overlapping
// with the merged PR — labels both (each via a distinct triage reason) and
// closes issue #42, not on PR-open, on the merge event.
func TestPostMergeFansOutAndClosesReferencedIssue(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Implements the thing.\n\nFixes #42",
		[]string{"portal/src/App.tsx", "shared/pkg.go"}, []int{21, 22})
	st.setConflicted(21)
	st.setFiles(22, "shared/pkg.go", "cmd/other.go", "portal/src/App.tsx")
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, stdout, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	assertLabeledExactly(t, st.labeledSnapshot(), 21, 22)

	st.mu.Lock()
	issueState := st.issueState[42]
	issueComments := append([]string(nil), st.issueComments[42]...)
	conflictComments := append([]string(nil), st.issueComments[21]...)
	overlapComments := append([]string(nil), st.issueComments[22]...)
	st.mu.Unlock()
	if issueState != "closed" {
		t.Fatalf("issue #42 state = %q, want closed", issueState)
	}
	if len(issueComments) != 1 || !strings.Contains(issueComments[0], "#20") {
		t.Fatalf("issue #42 comments = %v, want one mentioning pull request #20", issueComments)
	}
	if !strings.Contains(stdout, "labeled 2 pr(s)") {
		t.Fatalf("stdout = %q, want it to report 2 labeled", stdout)
	}
	if len(conflictComments) != 1 {
		t.Fatalf("PR #21 comments = %v, want one persisted conflict handoff", conflictComments)
	}
	conflict, ok := parsePostMergeRemediationHandoff(conflictComments[0])
	if !ok || conflict.DisplacingPullNumber != 20 || conflict.Reason != "conflicted" {
		t.Fatalf("PR #21 handoff = %+v, ok=%v; want displacing PR #20 and conflicted reason", conflict, ok)
	}
	if len(overlapComments) != 1 {
		t.Fatalf("PR #22 comments = %v, want one persisted overlap handoff", overlapComments)
	}
	overlap, ok := parsePostMergeRemediationHandoff(overlapComments[0])
	if !ok || overlap.DisplacingPullNumber != 20 ||
		strings.Join(overlap.OverlappingFiles, ",") != "portal/src/App.tsx,shared/pkg.go" {
		t.Fatalf("PR #22 handoff = %+v, ok=%v; want displacing PR #20 and both overlapping paths", overlap, ok)
	}
}

func TestPostMergeBookkeepingWarningIsBestEffort(t *testing.T) {
	st := newPostMergeServerState(20, "main", "fix", []string{"cmd/a.go"}, []int{21})
	st.setConflicted(21)
	st.labelStatus = http.StatusUnprocessableEntity
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, _, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q, want successful in-band merge despite bookkeeping warning", code, stderr)
	}
	if !strings.Contains(stderr, "warning: label pr #21") {
		t.Fatalf("stderr = %q, want sibling-label warning", stderr)
	}
}

func TestPostMergeDoesNotLabelWithoutPersistedHandoff(t *testing.T) {
	st := newPostMergeServerState(20, "main", "fix", []string{"cmd/a.go"}, []int{21})
	st.setConflicted(21)
	st.commentStatus = http.StatusInternalServerError
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, _, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q, want successful in-band merge despite bookkeeping warning", code, stderr)
	}
	if labeled := st.labeledSnapshot(); len(labeled) != 0 {
		t.Fatalf("labeled PRs = %v, want none when the remediation handoff could not be persisted", labeled)
	}
	if !strings.Contains(stderr, "persist remediation handoff on pr #21") {
		t.Fatalf("stderr = %q, want handoff persistence warning", stderr)
	}
}

func TestPostMergeHandoffIsIdempotentPerDisplacingPR(t *testing.T) {
	st := newPostMergeServerState(20, "main", "fix", nil, []int{21})
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	provider := mergePRTestServer{url: server.URL}.newGitHubProvider("test-token")
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	handoff := postMergeRemediationHandoff{DisplacingPullNumber: 20, Reason: "conflicted"}
	if err := persistPostMergeRemediationHandoff(context.Background(), provider, repo, 21, "goobers", handoff); err != nil {
		t.Fatalf("persist first handoff: %v", err)
	}
	handoff.Reason = "file-overlap:portal/src/App.tsx"
	handoff.OverlappingFiles = []string{"portal/src/App.tsx"}
	if err := persistPostMergeRemediationHandoff(context.Background(), provider, repo, 21, "goobers", handoff); err != nil {
		t.Fatalf("update handoff: %v", err)
	}

	st.mu.Lock()
	comments := append([]string(nil), st.issueComments[21]...)
	st.mu.Unlock()
	if len(comments) != 1 {
		t.Fatalf("PR #21 comments = %v, want one handoff updated in place", comments)
	}
	got, ok := parsePostMergeRemediationHandoff(comments[0])
	if !ok || got.Reason != handoff.Reason || strings.Join(got.OverlappingFiles, ",") != "portal/src/App.tsx" {
		t.Fatalf("updated handoff = %+v, ok=%v; want latest overlap diagnosis", got, ok)
	}
}

// TestPostMergeCleanDisjointSiblingsAreNotLabeled is #715's core acceptance
// criterion: merging a PR against a stack of 3 fully clean (mergeable, no
// file overlap) siblings labels NONE of them — 0 remediation runs, 0 CI
// restarts triggered for the others.
func TestPostMergeCleanDisjointSiblingsAreNotLabeled(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Implements the thing.",
		[]string{"cmd/a.go"}, []int{21, 22, 23})
	st.setFiles(21, "cmd/b.go")
	st.setFiles(22, "cmd/c.go")
	st.setFiles(23, "cmd/d.go")
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, stdout, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if labeled := st.labeledSnapshot(); len(labeled) != 0 {
		t.Fatalf("labeled PRs = %v, want none (all three siblings are clean and disjoint)", labeled)
	}
	if !strings.Contains(stdout, "labeled 0 pr(s)") || !strings.Contains(stdout, "3 clean siblings left untouched") {
		t.Fatalf("stdout = %q, want 0 labeled and 3 clean siblings reported", stdout)
	}
}

// TestPostMergeLabelsOnlyConflictedSibling proves triage discriminates
// per-PR: of two siblings, only the one GitHub reports as conflicted is
// labeled — the clean one is left untouched in the SAME run.
func TestPostMergeLabelsOnlyConflictedSibling(t *testing.T) {
	st := newPostMergeServerState(20, "main", "fix", []string{"cmd/a.go"}, []int{21, 22})
	st.setConflicted(21)
	st.setFiles(22, "cmd/unrelated.go")
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, _, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	assertLabeledExactly(t, st.labeledSnapshot(), 21)
}

// TestPostMergeLabelsOnlyFileOverlappingSibling is the file-overlap half of
// the same discrimination proof: #22 shares a file with the merged PR, #21
// does not — only #22 is labeled.
func TestPostMergeLabelsOnlyFileOverlappingSibling(t *testing.T) {
	st := newPostMergeServerState(20, "main", "fix", []string{"internal/shared/x.go"}, []int{21, 22})
	st.setFiles(21, "cmd/unrelated.go")
	st.setFiles(22, "internal/shared/x.go", "internal/shared/y.go")
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, _, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	assertLabeledExactly(t, st.labeledSnapshot(), 22)
}

// TestPostMergeUnknownMergeableIsNotTreatedAsConflicted: GitHub reporting
// mergeable=null (still computing — the normal state right after a merge
// just changed the base) must NOT be treated as a conflict. A clean,
// disjoint sibling with an unresolved mergeable state stays unlabeled.
func TestPostMergeUnknownMergeableIsNotTreatedAsConflicted(t *testing.T) {
	st := newPostMergeServerState(20, "main", "fix", []string{"cmd/a.go"}, []int{21})
	st.setMergeableUnknown(21)
	st.setFiles(21, "cmd/unrelated.go")
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, _, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if labeled := st.labeledSnapshot(); len(labeled) != 0 {
		t.Fatalf("labeled PRs = %v, want none (null mergeable is 'unknown', not 'conflicted')", labeled)
	}
}

// TestPostMergeConservativelyLabelsOnMergeableCheckFailure: a provider
// error on the mergeable check must not be silently treated as "clean" — it
// conservatively labels, since a false negative here risks a real conflict
// slipping through unlabeled.
func TestPostMergeConservativelyLabelsOnMergeableCheckFailure(t *testing.T) {
	st := newPostMergeServerState(20, "main", "fix", []string{"cmd/a.go"}, []int{21})
	st.setFiles(21, "cmd/unrelated.go")
	server := newPostMergeServer(t, "your-org", "your-repo", st)

	// Wrap the fixture's own handler to break the per-PR detail endpoint
	// (the mergeable check) specifically with a 404 — simplest way to force
	// PullRequestMergeable to fail without touching any other endpoint. A
	// non-retryable status (unlike 5xx, which send() retries with backoff)
	// keeps this test fast.
	brokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/your-org/your-repo/pulls/21" && r.Method == http.MethodGet {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(brokenServer.Close)
	root, _ := postMergeEnv(t, brokenServer.URL, false, map[string]string{"pullNumber": "20"})

	code, _, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "conservatively labeling needs-remediation") {
		t.Fatalf("stderr = %q, want a warning about conservative labeling", stderr)
	}
	assertLabeledExactly(t, st.labeledSnapshot(), 21)
}

// TestPostMergeReusesSiblingCacheForFileOverlap proves #715's stated
// synergy with #523: when the instance's sibling-context cache already has
// a fresh (matching head SHA) entry for a sibling — the common case, since
// post-merge runs at the end of the SAME merge-review run gather-sibling-
// context started — the file-overlap check reuses it instead of an extra
// PullRequestFiles call.
func TestPostMergeReusesSiblingCacheForFileOverlap(t *testing.T) {
	st := newPostMergeServerState(20, "main", "fix", []string{"cmd/shared.go"}, []int{21})
	st.setHeadSHA(21, "cachedsha21")
	// Deliberately leave the live /files endpoint serving a DIFFERENT,
	// non-overlapping file — if the cache is (wrongly) bypassed, this test
	// would see a files request and, since the live fixture doesn't
	// overlap, the labeling assertion alone wouldn't distinguish reuse from
	// a fresh miss. The request counter is the real proof.
	st.setFiles(21, "cmd/unrelated-live-fetch.go")
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	// Seed the sibling cache as gather-sibling-context would have, earlier
	// in the same run: PR #21 at head "cachedsha21" touches cmd/shared.go —
	// the same file the merged PR #20 touches, so a cache-driven overlap
	// hit is the ONLY way this test's labeling assertion can pass.
	schedulerDir := layoutFor(root).SchedulerDir()
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	cacheFile := siblingCacheFile{Entries: map[string]siblingCacheEntry{
		"21": {HeadSHA: "cachedsha21", CheckState: "passing", Files: []string{"cmd/shared.go"}},
	}}
	data, err := json.Marshal(cacheFile)
	if err != nil {
		t.Fatalf("marshal cache fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(schedulerDir, siblingCacheFileName), data, 0o644); err != nil {
		t.Fatalf("write cache fixture: %v", err)
	}

	code, _, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	assertLabeledExactly(t, st.labeledSnapshot(), 21)

	st.mu.Lock()
	filesReqs := st.filesRequests[21]
	st.mu.Unlock()
	if filesReqs != 0 {
		t.Fatalf("PR #21 files requests = %d, want 0 (cache hit should skip the live fetch entirely)", filesReqs)
	}
}

// TestPostMergeNoIssueReferenceIsNotAnError proves a merged PR whose body
// references no backlog issue still succeeds — triage still runs (a clean
// sibling stays unlabeled), but there is simply nothing to close.
func TestPostMergeNoIssueReferenceIsNotAnError(t *testing.T) {
	st := newPostMergeServerState(20, "main", "A manual fix, not tied to a backlog issue.",
		[]string{"cmd/a.go"}, []int{21})
	st.setFiles(21, "cmd/unrelated.go")
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, stdout, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "closed 0 issue") {
		t.Fatalf("stdout = %q, want a mention of 0 issues closed", stdout)
	}
	if labeled := st.labeledSnapshot(); len(labeled) != 0 {
		t.Fatalf("labeled PRs = %v, want none (the one sibling is clean)", labeled)
	}
}

// TestPostMergeRefusesWithoutCapability proves post-merge fails closed
// before any provider call when either required capability is absent.
func TestPostMergeRefusesWithoutCapability(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, nil)
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

// TestReferencedIssueNumbers exercises the widened backstop parser (#980): it
// covers every closing form closingIssueNumbers does AND the non-closing
// "Implements #N" convention a structured PR body writes, while still
// ignoring a bare cross-reference mention that carries no directed keyword.
func TestReferencedIssueNumbers(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"implements", "## Summary\n\nImplements #774: **Convert logs**.", []string{"774"}},
		{"implemented-past-tense", "Implemented #12 as specced.", []string{"12"}},
		{"fixes-footer", "Body.\n\n---\nFixes #42", []string{"42"}},
		{"implements-plus-fixes-collapses", "Implements #774.\n\n---\nFixes #774", []string{"774"}},
		{"bare-mention-ignored", "Implements #5 (see also #700).", []string{"5"}},
		{"no-reference", "Just a manual change, no backlog issue.", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := referencedIssueNumbers(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("referencedIssueNumbers(%q) = %v, want %v", tc.body, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("referencedIssueNumbers(%q) = %v, want %v", tc.body, got, tc.want)
				}
			}
		})
	}
}
