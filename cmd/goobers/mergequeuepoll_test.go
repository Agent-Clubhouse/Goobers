package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/boundedwait"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// mergeQueuePollServerState scripts one pull request's live state across
// repeated GET .../pulls/9 polls (issue #758's merge-queue-poll): the first
// pendingCalls polls report open+unmerged (still queued); the poll after
// that reports terminalState/terminalMerged. A test that never wants a
// terminal state (the timeout case) sets pendingCalls very high relative to
// its own short pollTimeoutSeconds input.
type mergeQueuePollServerState struct {
	mu sync.Mutex

	pendingCalls   int
	terminalState  string // "open" or "closed"
	terminalMerged bool
	headBranch     string
	headSHA        string

	// pendingEntryAbsent makes the pending-phase polls report NO merge queue
	// entry rather than a live one — issue #885's eviction shape, since
	// GitHub leaves an evicted pull request open and simply removes its
	// entry. Left unset, pending polls report a normal queued entry.
	pendingEntryAbsent bool

	// graphqlScript, when non-empty, drives the queue-entry poll from an
	// explicit per-call sequence instead of the pendingCalls counter, so a
	// test can script a state TRANSITION the counter cannot express (#924
	// needs absent-then-merged: the exact instant a queue merge removes the
	// entry before `merged` has propagated to the replica the read lands on).
	// Recognized values: "pending", "absent", "merged", "closed". The last
	// entry repeats for any further polls.
	graphqlScript []string

	pollCalls     int
	graphqlPolls  int
	pullListCalls int
	deleteCalls   int
	labelCalls    int
	commentCalls  int
	// commentPostCalls counts only comment CREATION (POST). commentCalls
	// lumps in PollPullRequest's own benign comments-list GET, so it cannot
	// answer "did we post a comment onto this pull request?" — which is the
	// question #924's assertions actually need.
	commentPostCalls int
	commentBodies    []string
	labelStatus      int // non-zero forces the labels endpoint to fail
	labels           []string
	// optOutAfterGraphQLPolls delays labels until the watcher has already
	// observed this many queue polls.
	optOutAfterGraphQLPolls int
	dequeueCalls            int
	dequeueFails            bool
	dequeueFailures         int
}

func TestMergeQueuePollBackoffJittersWithinCappedExponentialRange(t *testing.T) {
	const base = 10 * time.Second
	const max = 100 * time.Second
	cases := []struct {
		attempt int
		ceiling time.Duration
	}{
		{0, 10 * time.Second},
		{1, 20 * time.Second},
		{2, 40 * time.Second},
		{3, 80 * time.Second},
		{4, 100 * time.Second},
		{100, 100 * time.Second},
	}
	for _, tc := range cases {
		for range 100 {
			got := mergeQueuePollBackoff(base, max, tc.attempt)
			if floor := tc.ceiling / 2; got < floor || got > tc.ceiling {
				t.Errorf("mergeQueuePollBackoff(%s, %s, %d) = %s, want range [%s, %s]", base, max, tc.attempt, got, floor, tc.ceiling)
			}
		}
	}
}

func newMergeQueuePollServer(t *testing.T, owner, repo string, st *mergeQueuePollServerState) *httptest.Server {
	t.Helper()
	if st.headBranch == "" {
		st.headBranch = "goobers/implementation/run-9"
	}
	if st.headSHA == "" {
		st.headSHA = "head9sha"
	}
	prefix := "/repos/" + owner + "/" + repo
	mux := http.NewServeMux()

	mux.HandleFunc(prefix+"/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.pollCalls++
		terminal := st.pollCalls > st.pendingCalls
		st.mu.Unlock()

		state := "open"
		merged := false
		if terminal {
			state = st.terminalState
			merged = st.terminalMerged
		}
		writeFakeJSON(w, map[string]interface{}{
			"number": 9, "state": state, "merged": merged,
			"head": map[string]interface{}{"ref": st.headBranch, "sha": st.headSHA,
				"repo": map[string]interface{}{"name": repo, "html_url": "https://github.com/" + owner + "/" + repo, "owner": map[string]string{"login": owner}}},
			"base": map[string]interface{}{"ref": "main", "sha": "basesha"},
		})
	})
	// The queue-entry poll itself is GraphQL (issue #885) — the merge queue
	// entry is the only surface that distinguishes "still queued" from "no
	// longer queued", and REST exposes nothing equivalent. The REST
	// .../pulls/9 handler above still serves PollPullRequest, which the
	// merged path calls separately to resolve branch-cleanup details.
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode graphql request: %v", err)
			http.Error(w, "invalid graphql request", http.StatusBadRequest)
			return
		}
		if strings.Contains(body.Query, "dequeuePullRequest(input:") {
			st.mu.Lock()
			st.dequeueCalls++
			fails := st.dequeueFails
			transientFailure := st.dequeueFailures > 0
			if transientFailure {
				st.dequeueFailures--
			}
			st.mu.Unlock()
			if transientFailure {
				writeFakeJSON(w, map[string]interface{}{
					"data": nil, "errors": []map[string]string{{"type": "INTERNAL", "message": "temporary failure"}},
				})
				return
			}
			if fails {
				writeFakeJSON(w, map[string]interface{}{
					"data": nil, "errors": []map[string]string{{"type": "UNPROCESSABLE", "message": "merge queue entry already resolved"}},
				})
				return
			}
			writeFakeJSON(w, map[string]interface{}{"data": map[string]interface{}{
				"dequeuePullRequest": map[string]interface{}{"clientMutationId": nil},
			}})
			return
		}

		st.mu.Lock()
		st.graphqlPolls++
		call := st.graphqlPolls
		terminal := st.graphqlPolls > st.pendingCalls
		terminalState, terminalMerged, entryAbsent := st.terminalState, st.terminalMerged, st.pendingEntryAbsent
		script := st.graphqlScript
		labels := append([]string(nil), st.labels...)
		showLabels := len(labels) > 0 && call > st.optOutAfterGraphQLPolls
		st.mu.Unlock()

		labelNodes := make([]map[string]string, 0, len(labels))
		if showLabels {
			for _, label := range labels {
				labelNodes = append(labelNodes, map[string]string{"name": label})
			}
		}

		// Explicitly scripted sequence (#924) takes precedence over the
		// pendingCalls counter; the last step repeats once exhausted.
		if len(script) > 0 {
			step := script[len(script)-1]
			if call <= len(script) {
				step = script[call-1]
			}
			pr := map[string]interface{}{
				"id": "PR_node9", "state": "OPEN", "merged": false, "mergeCommit": nil,
				"mergeQueueEntry": nil, "labels": map[string]interface{}{"nodes": labelNodes},
			}
			switch step {
			case "pending":
				pr["mergeQueueEntry"] = map[string]interface{}{"state": "QUEUED", "position": 1}
			case "absent":
				// Open, unmerged, no entry — indistinguishable on a single
				// read from both a real eviction and a just-landed merge.
			case "merged":
				pr["state"] = "MERGED"
				pr["merged"] = true
				pr["mergeCommit"] = map[string]interface{}{"oid": "queuemergesha"}
			case "closed":
				pr["state"] = "CLOSED"
			default:
				t.Errorf("unknown graphqlScript step %q", step)
			}
			writeFakeJSON(w, map[string]interface{}{"data": map[string]interface{}{
				"repository": map[string]interface{}{"pullRequest": pr},
			}})
			return
		}

		pr := map[string]interface{}{
			"id": "PR_node9", "state": "OPEN", "merged": false, "mergeCommit": nil,
			"labels": map[string]interface{}{"nodes": labelNodes},
		}
		switch {
		case terminal && terminalMerged:
			pr["state"] = "MERGED"
			pr["merged"] = true
			pr["mergeCommit"] = map[string]interface{}{"oid": "queuemergesha"}
		case terminal && terminalState == "closed":
			pr["state"] = "CLOSED"
		}
		// The entry exists only during the pending phase. Once the scripted
		// state goes terminal, it is gone — which, for a pull request that
		// is still OPEN and unmerged, is exactly how GitHub presents an
		// eviction (#885).
		pr["mergeQueueEntry"] = nil
		if !terminal && !entryAbsent {
			pr["mergeQueueEntry"] = map[string]interface{}{"state": "QUEUED", "position": 1}
		}
		writeFakeJSON(w, map[string]interface{}{"data": map[string]interface{}{
			"repository": map[string]interface{}{"pullRequest": pr},
		}})
	})
	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.pullListCalls++
		st.mu.Unlock()
		writeFakeJSON(w, []map[string]interface{}{})
	})
	mux.HandleFunc(prefix+"/pulls/9/reviews", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, []map[string]interface{}{})
	})
	mux.HandleFunc(prefix+"/commits/"+st.headSHA+"/status", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"state": "success", "statuses": []map[string]interface{}{}})
	})
	mux.HandleFunc(prefix+"/commits/"+st.headSHA+"/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"check_runs": []map[string]interface{}{
			{"name": "make-ci", "status": "completed", "conclusion": "success"},
		}})
	})
	// Serves both PollPullRequest's own comments-list GET (queried while
	// resolving the merged outcome's branch-cleanup details) and
	// UpdateWorkItem's comment-creation POST (the eviction side effect) —
	// one registration per net/http.ServeMux pattern, branching on method.
	mux.HandleFunc(prefix+"/issues/9/comments", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.commentCalls++
		st.mu.Unlock()
		if r.Method == http.MethodGet {
			writeFakeJSON(w, []map[string]interface{}{})
			return
		}
		st.mu.Lock()
		st.commentPostCalls++
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			st.mu.Unlock()
			t.Errorf("decode posted comment: %v", err)
			http.Error(w, "invalid comment", http.StatusBadRequest)
			return
		}
		st.commentBodies = append(st.commentBodies, body.Body)
		st.mu.Unlock()
		writeFakeJSON(w, map[string]interface{}{"id": 1})
	})
	mux.HandleFunc(prefix+"/git/refs/heads/"+st.headBranch, func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.deleteCalls++
		st.mu.Unlock()
		if r.Method != http.MethodDelete {
			t.Errorf("branch request method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(prefix+"/issues/9", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"number": 9, "state": "open", "html_url": "https://github.com/" + owner + "/" + repo + "/issues/9"})
	})
	mux.HandleFunc(prefix+"/issues/9/labels", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.labelCalls++
		status := st.labelStatus
		st.mu.Unlock()
		if status != 0 {
			http.Error(w, "label failed", status)
			return
		}
		writeFakeJSON(w, []map[string]string{})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// mergeQueuePollEnv mirrors mergePREnv: instance root, run/workflow
// identity, both capability tokens merge-queue-poll needs
// (github:pr:merge for the poll itself, github:issues:write for the
// eviction-labeling side effect), and declared Task.Inputs.
func mergeQueuePollEnv(t *testing.T, serverURL string, inputs map[string]string) (instanceRoot, workDir string) {
	t.Helper()
	instanceRoot = initDemo(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: serverURL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-merge-1")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_MERGE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_BRANCH_DELETE", "test-token")
	t.Setenv(executor.RepoProviderEnvVar, "github")
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	for k, v := range inputs {
		t.Setenv("GOOBERS_INPUT_"+strings.ToUpper(k), v)
	}
	workDir = t.TempDir()
	t.Chdir(workDir)
	return instanceRoot, workDir
}

func readQueueResult(t *testing.T, dir string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "queue-result.json"))
	if err != nil {
		t.Fatalf("read queue-result.json: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal queue-result.json: %v", err)
	}
	return result
}

// TestMergeQueuePollReportsMergedAndCleansUpBranch is #758's queue-merged
// path: once the queue actually merges the pull request, this stage
// reports queueOutcome=merged and runs the same branch cleanup merge-pr's
// direct-merge path already does.
func TestMergeQueuePollReportsMergedAndCleansUpBranch(t *testing.T) {
	st := &mergeQueuePollServerState{pendingCalls: 1, terminalState: "closed", terminalMerged: true}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "merged" {
		t.Fatalf("result = %+v, want queueOutcome=merged", result)
	}
	if result["selectedNumber"] != "9" {
		t.Fatalf("result = %+v, want selectedNumber=9", result)
	}
	if st.deleteCalls != 1 || st.pullListCalls != 1 {
		t.Fatalf("cleanup calls = delete:%d list:%d, want 1 each", st.deleteCalls, st.pullListCalls)
	}
	if result["branchCleanup"] != "deleted" {
		t.Fatalf("result = %+v, want branchCleanup=deleted", result)
	}
	if st.labelCalls != 0 {
		t.Fatalf("label calls = %d, want 0 (a merged pull request must never be labeled needs-remediation)", st.labelCalls)
	}
}

func TestMergeQueuePollDequeuesAfterLateOptOutWithoutRemediation(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript:           []string{"pending", "pending"},
		pendingCalls:            1_000_000,
		terminalState:           "open",
		labels:                  []string{noMergeReviewLabel},
		optOutAfterGraphQLPolls: 1,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != mergeReviewOptOutOutcome {
		t.Fatalf("result = %+v, want queueOutcome=%s", result, mergeReviewOptOutOutcome)
	}
	if st.dequeueCalls != 1 {
		t.Fatalf("dequeue calls = %d, want 1 after the live opt-out", st.dequeueCalls)
	}
	if st.labelCalls != 0 || st.commentPostCalls != 0 || st.deleteCalls != 0 {
		t.Fatalf("post-opt-out mutations = labels:%d comments:%d branch deletes:%d, want none", st.labelCalls, st.commentPostCalls, st.deleteCalls)
	}
}

func TestMergeQueuePollRetriesTransientDequeueFailure(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript:           []string{"pending", "pending", "pending"},
		pendingCalls:            1_000_000,
		terminalState:           "open",
		labels:                  []string{noMergeReviewLabel},
		optOutAfterGraphQLPolls: 1,
		dequeueFailures:         1,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != mergeReviewOptOutOutcome {
		t.Fatalf("result = %+v, want queueOutcome=%s", result, mergeReviewOptOutOutcome)
	}
	if st.dequeueCalls != 2 {
		t.Fatalf("dequeue calls = %d, want retry after the transient failure", st.dequeueCalls)
	}
	if st.labelCalls != 0 || st.commentPostCalls != 0 || st.deleteCalls != 0 {
		t.Fatalf("post-opt-out mutations = labels:%d comments:%d branch deletes:%d, want none", st.labelCalls, st.commentPostCalls, st.deleteCalls)
	}
}

func TestMergeQueuePollResolvesMergeThatRacesOptOutDequeue(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript:           []string{"pending", "pending", "merged"},
		pendingCalls:            0,
		terminalState:           "closed",
		terminalMerged:          true,
		labels:                  []string{noMergeReviewLabel},
		optOutAfterGraphQLPolls: 1,
		dequeueFails:            true,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "merged" {
		t.Fatalf("result = %+v, want queueOutcome=merged after the dequeue race", result)
	}
	if st.dequeueCalls != 1 || st.labelCalls != 0 || st.commentPostCalls != 0 {
		t.Fatalf("race calls = dequeue:%d labels:%d comments:%d, want 1/0/0", st.dequeueCalls, st.labelCalls, st.commentPostCalls)
	}
}

func TestMergeQueuePollConfirmsAbsentAfterFailedDequeue(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript:           []string{"pending", "pending", "absent", "merged"},
		pendingCalls:            0,
		terminalState:           "closed",
		terminalMerged:          true,
		labels:                  []string{noMergeReviewLabel},
		optOutAfterGraphQLPolls: 1,
		dequeueFails:            true,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "merged" {
		t.Fatalf("result = %+v, want queueOutcome=merged after one ambiguous absent read", result)
	}
	if st.dequeueCalls != 1 || st.graphqlPolls != 4 {
		t.Fatalf("queue calls = dequeue:%d polls:%d, want 1/4", st.dequeueCalls, st.graphqlPolls)
	}
	if st.labelCalls != 0 || st.commentPostCalls != 0 {
		t.Fatalf("post-race remediation mutations = labels:%d comments:%d, want none", st.labelCalls, st.commentPostCalls)
	}
	if st.deleteCalls != 1 {
		t.Fatalf("branch deletes = %d, want merged bookkeeping to clean up the branch", st.deleteCalls)
	}
}

// TestMergeQueuePollReportsEvictedAndLabelsForRemediation is #758's headline
// acceptance criterion: an evicted pull request is labeled
// goobers:needs-remediation with an explanatory comment BEFORE the stage
// reports queueOutcome=evicted — the routing itself, not just the report.
func TestMergeQueuePollReportsEvictedAndLabelsForRemediation(t *testing.T) {
	st := &mergeQueuePollServerState{pendingCalls: 1, terminalState: "closed", terminalMerged: false}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "evicted" {
		t.Fatalf("result = %+v, want queueOutcome=evicted", result)
	}
	if st.labelCalls != 1 {
		t.Fatalf("label calls = %d, want 1 (eviction must apply goobers:needs-remediation)", st.labelCalls)
	}
	if st.commentCalls != 1 {
		t.Fatalf("comment calls = %d, want 1 (eviction must explain why)", st.commentCalls)
	}
	if st.deleteCalls != 0 {
		t.Fatalf("branch delete calls = %d, want 0 (an evicted pull request was never merged, nothing to clean up)", st.deleteCalls)
	}
	if _, ok := result["mergeSha"]; ok {
		t.Fatalf("result = %+v, want no mergeSha for an evicted pull request", result)
	}
}

// TestMergeQueuePollDetectsEvictionWhileThePullRequestStaysOpen is issue
// #885's headline regression, and the shape a REAL merge-queue eviction
// takes: GitHub does not close an evicted pull request, it leaves it open
// and removes its queue entry.
//
// The old REST classification only checked pr.State == "closed", so this
// case reported Pending forever — the poll ran to its timeout and
// goobers:needs-remediation, #758's own acceptance criterion, could never
// be applied. pr-remediation was therefore unreachable by the one event it
// exists to handle.
func TestMergeQueuePollDetectsEvictionWhileThePullRequestStaysOpen(t *testing.T) {
	st := &mergeQueuePollServerState{
		// One poll with a live entry, then the entry disappears while the
		// pull request stays open and unmerged.
		pendingCalls: 1, terminalState: "open", terminalMerged: false, pendingEntryAbsent: false,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "evicted" {
		t.Fatalf("result = %+v, want queueOutcome=evicted for an open pull request dropped from the queue", result)
	}
	if st.labelCalls != 1 {
		t.Fatalf("label calls = %d, want 1 — the eviction must route to remediation, which is the whole point", st.labelCalls)
	}
	if st.deleteCalls != 0 {
		t.Fatalf("branch delete calls = %d, want 0 (an evicted pull request was never merged)", st.deleteCalls)
	}
}

// TestMergeQueuePollDoesNotCallAJustLandedMergeAnEviction is #924, and it is
// the sequence that actually happened to PR #922 on 2026-07-19.
//
// A queue merge removes the entry at the instant it lands. PollMergeQueueEntry
// does check pr.Merged before it reports Absent — but that GraphQL read is not
// atomic, so a poll can land on a replica where the entry is already gone and
// `merged` has not yet flipped true. On that single read a successful merge is
// byte-for-byte indistinguishable from an eviction.
//
// Committing on that read is what went wrong live: PR #922 merged at 04:04:16Z
// (merge commit 4f1a7665, merge_group CI green at 04:03:44Z) and its own run
// posted a "its combined build against the projected merge state failed"
// comment plus goobers:needs-remediation onto it one second later, at
// 04:04:17Z. The pull request is genuinely on main and is permanently
// mislabeled, because nothing downstream ever clears either.
//
// So the scripted sequence here is the race verbatim: entry visible, then the
// absent instant, then merged. The assertions that matter are the side effects
// — a merge must produce zero label calls, because the label is the damage.
func TestMergeQueuePollDoesNotCallAJustLandedMergeAnEviction(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript: []string{"pending", "absent", "merged"},
		// PollPullRequest (the merged path's branch-cleanup lookup) reads
		// REST, which is driven separately by pendingCalls; go terminal
		// immediately so cleanup resolves.
		pendingCalls: 0, terminalState: "closed", terminalMerged: true,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "merged" {
		t.Fatalf("result = %+v, want queueOutcome=merged — the entry vanished because the queue MERGED it, "+
			"and the very next poll says so; committing to an eviction on the single absent read is #924", result)
	}
	if st.labelCalls != 0 {
		t.Fatalf("label calls = %d, want 0 — labeling a merged pull request goobers:needs-remediation is the "+
			"damage this test exists to prevent; nothing downstream ever removes it", st.labelCalls)
	}
	if st.commentPostCalls != 0 {
		t.Fatalf("comment POSTs = %d, want 0 — no false 'build failed' comment on a pull request that merged", st.commentPostCalls)
	}
}

// TestMergeQueuePollStillDetectsAnEvictionThatPersists is the other half of
// #924: the confirmation requirement must not blunt real eviction detection.
// A genuine eviction leaves the pull request open and unmerged indefinitely,
// so absence persists across polls and still routes to remediation — one extra
// poll interval later, which is negligible against the stage's own budget.
func TestMergeQueuePollStillDetectsAnEvictionThatPersists(t *testing.T) {
	st := &mergeQueuePollServerState{
		// Absent from the second poll onward and never merging — a real
		// eviction, distinguished from the case above only by persistence.
		graphqlScript: []string{"pending", "absent", "absent"},
		pendingCalls:  1, terminalState: "open", terminalMerged: false,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "evicted" {
		t.Fatalf("result = %+v, want queueOutcome=evicted — persistent absence IS an eviction, and #924's "+
			"confirmation must not stop it being detected", result)
	}
	if st.labelCalls != 1 {
		t.Fatalf("label calls = %d, want 1 — routing an eviction to remediation is the acceptance criterion", st.labelCalls)
	}
}

// TestMergeQueuePollToleratesAnAbsentEntryBeforeItHasSeenOne guards the
// other side of #885: an absent entry is ALSO what the gap between
// merge-pr's enqueue and the entry becoming visible looks like. Reading
// that as an eviction would label a perfectly healthy just-enqueued pull
// request needs-remediation. Before any entry has been seen, absence is
// tolerated for a grace window — so a poll whose budget expires inside that
// window reports timeout, never eviction. Timeout still leaves the labeled,
// commented trail required by #944.
func TestMergeQueuePollToleratesAnAbsentEntryBeforeItHasSeenOne(t *testing.T) {
	st := &mergeQueuePollServerState{
		// No entry ever appears, and the pull request stays open.
		pendingCalls: 1_000_000, terminalState: "open", terminalMerged: false, pendingEntryAbsent: true,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		// A poll budget far shorter than the grace window, so the run ends
		// while absence is still being tolerated.
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "20ms",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "timeout" {
		t.Fatalf("result = %+v, want queueOutcome=timeout — an entry that is not visible YET is not an eviction", result)
	}
	if st.labelCalls != 1 || st.commentPostCalls != 1 {
		t.Fatalf("label/comment calls = %d/%d, want 1/1 — timeout must remain visible even when absence is still inside the propagation grace window", st.labelCalls, st.commentPostCalls)
	}
}

// TestMergeQueuePollEvictionLabelFailureFailsTheStage proves the routing IS
// the acceptance criterion: a failure to apply goobers:needs-remediation on
// an evicted pull request must fail the stage (exit 1, with a classified
// error in the result file via failProviderStage — the same convention
// every other provider-chain subcommand's genuine failures follow), not
// silently report evicted with no actual routing having happened. 422 (not
// 5xx) so classifyProviderError treats it as non-retryable and the
// provider's own internal retry-with-backoff never kicks in, keeping the
// test fast.
func TestMergeQueuePollEvictionLabelFailureFailsTheStage(t *testing.T) {
	st := &mergeQueuePollServerState{pendingCalls: 0, terminalState: "closed", terminalMerged: false, labelStatus: http.StatusUnprocessableEntity}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code == 0 {
		t.Fatalf("code = 0, stderr = %q, want a stage failure when eviction labeling fails", stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "queue-result.json"))
	if err != nil {
		t.Fatalf("read queue-result.json: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal queue-result.json: %v", err)
	}
	if _, ok := result["errorCode"]; !ok {
		t.Fatalf("result = %+v, want a classified errorCode (failProviderStage's convention), not a queueOutcome key", result)
	}
	if _, ok := result["queueOutcome"]; ok {
		t.Fatalf("result = %+v, want no queueOutcome written — the routing failed, so no outcome was actually determined", result)
	}
}

// TestMergeQueuePollTimesOutWhenStillPending is #758's third outcome
// (mirroring ci-status's own OutcomeTimeout, #239): a pull request that
// never resolves within this stage's own bounded poll reports
// queueOutcome=timeout, distinct from both merged and evicted, with a
// human-visible remediation label and explanatory comment (#944).
func TestMergeQueuePollTimesOutWhenStillPending(t *testing.T) {
	st := &mergeQueuePollServerState{pendingCalls: 1_000_000, terminalState: "open", terminalMerged: false}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "20ms",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q, want exit 0 for a timeout (not a stage failure)", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "timeout" {
		t.Fatalf("result = %+v, want queueOutcome=timeout", result)
	}
	reason, _ := result["reason"].(string)
	if !strings.Contains(reason, "timed out") || !strings.Contains(reason, "still pending") {
		t.Fatalf("result reason = %q, want the timeout explained", reason)
	}
	if st.labelCalls != 1 || st.commentPostCalls != 1 {
		t.Fatalf("label/comment calls = %d/%d, want 1/1 so a timed-out PR remains visible", st.labelCalls, st.commentPostCalls)
	}
	if len(st.commentBodies) != 1 || !strings.Contains(st.commentBodies[0], "timed out") || !strings.Contains(st.commentBodies[0], needsRemediationLabel) {
		t.Fatalf("comments = %q, want one human-visible timeout explanation naming %s", st.commentBodies, needsRemediationLabel)
	}
	if st.deleteCalls != 0 {
		t.Fatalf("branch delete calls = %d, want 0 for a timeout", st.deleteCalls)
	}
	ledgerPath := filepath.Join(layoutFor(root).SchedulerDir(), postMergeReconcileLedgerFile)
	ledger, err := readPostMergeReconcileLedger(ledgerPath)
	if err != nil {
		t.Fatalf("read post-merge reconcile ledger: %v", err)
	}
	entry := ledger.Entries[postMergeReconcileKey(postMergeTestRepo(), "9")]
	if entry.State != postMergeReconcilePending {
		t.Fatalf("reconcile entry = %+v, want pending timeout for PR #9", entry)
	}
}

// TestMergeQueuePollBudgetStaysInsideTheStageDeadline is issue #884's unit
// regression. The default poll timeout (30m) is three times the shell
// executor's default stage timeout (10m), so an unclamped poll never
// reaches its own timeout branch — it is SIGKILLed first, never writes
// queue-result.json, and queue-gate then reads the missing queueOutcome as
// fail, journaling the whole run as FAILED for a pull request that was in
// fact successfully enqueued.
//
// The budget must therefore be strictly less than the stage deadline, with
// room for the final round trip and the result-file write.
func TestMergeQueuePollBudgetStaysInsideTheStageDeadline(t *testing.T) {
	cases := []struct {
		name  string
		stage time.Duration
	}{
		{"executor default", executor.DefaultTimeout},
		{"long declared timeout", 35 * time.Minute},
		{"short declared timeout", 90 * time.Second},
		{"degenerate timeout", 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget := boundedwait.MergeQueuePollBudget(tc.stage)
			if budget <= 0 {
				t.Fatalf("budget = %s, want a positive poll budget", budget)
			}
			if budget >= tc.stage {
				t.Fatalf("budget = %s, want strictly less than the %s stage deadline — a poll that outlasts its stage is SIGKILLed before it can write a result", budget, tc.stage)
			}
		})
	}
	// The specific pairing that caused the live failure.
	if got := boundedwait.MergeQueuePollBudget(executor.DefaultTimeout); got >= executor.DefaultPollTimeout {
		t.Fatalf("budget under the default stage timeout = %s, want it to clamp the %s default poll timeout down", got, executor.DefaultPollTimeout)
	}
}

// TestMergeQueuePollReadsStageTimeoutFromDeclaredInput proves the clamp
// tracks whatever timeout the workflow actually declares, rather than
// assuming the executor default. This is what makes the fix survive the
// hand-maintained instance config: a stage declaring a longer timeout gets
// a correspondingly longer poll with no rebuild.
func TestMergeQueuePollReadsStageTimeoutFromDeclaredInput(t *testing.T) {
	t.Setenv(executor.InputEnvVar(executor.InputTimeout), "35m")
	if got, want := stageTimeout(), 35*time.Minute; got != want {
		t.Fatalf("stageTimeout() = %s, want %s (the declared input)", got, want)
	}
	if budget := boundedwait.MergeQueuePollBudget(stageTimeout()); budget <= executor.DefaultTimeout {
		t.Fatalf("budget = %s, want more than the %s executor default once the stage declares 35m", budget, executor.DefaultTimeout)
	}
}

// TestMergeQueuePollClampsPollTimeoutToStageBudget is the end-to-end half:
// a stage that declares a short timeout and a long poll still exits 0 with
// a written result, rather than running past its own deadline.
func TestMergeQueuePollClampsPollTimeoutToStageBudget(t *testing.T) {
	st := &mergeQueuePollServerState{pendingCalls: 1_000_000, terminalState: "open", terminalMerged: false}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms",
		// A poll timeout far past the stage timeout — the exact shape of
		// the live defect, where the 30m default ran inside a 10m stage.
		//
		// The stage timeout itself needs its own margin: MergeQueuePollBudget
		// halves whatever's left after its minute-scale minimum margin
		// swallows any stage under a minute, so this value passes straight
		// through as (roughly) half its own duration as the poll loop's
		// internal deadline. An 80ms stage (40ms internal deadline) left
		// almost no room for scheduling jitter — on a loaded CI runner the
		// process can lose 40ms+ to scheduling alone before its own clamped
		// deadline check ever runs, so the shared per-command context
		// (providerCommandContext, ~90% of the stage) expires first
		// mid-HTTP-call instead of the poll loop's own deadline exiting
		// cleanly — "context deadline exceeded" instead of a clamped
		// queueOutcome=timeout. 3s keeps the test itself fast (well under the
		// 30s failsafe below) while giving the internal ~1.5s clamped budget
		// enough headroom to survive CI contention (see boundedwait_test.go's
		// "merge queue degenerate stage" case for the same stage/2 shape at
		// a larger scale).
		"pollTimeoutSeconds": "30m",
		"timeout":            "3s",
	})

	done := make(chan struct{})
	var code int
	var stderr string
	go func() {
		defer close(done)
		code, _, stderr = runArgs(t, "merge-queue-poll", root)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("merge-queue-poll did not return — it polled past its own stage deadline instead of clamping to it")
	}

	if code != 0 {
		t.Fatalf("code = %d, stderr = %q, want exit 0 for a clamped timeout (not a stage failure)", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "timeout" {
		t.Fatalf("result = %+v, want queueOutcome=timeout written before the stage deadline", result)
	}
}

// adoPRDetailState scripts the single ADO pull-request detail merge-queue-poll's
// ADO land oracle reads (PollMergeQueueEntry → getPullRequestDetail). workItemHits
// records any /_apis/wit/ path touched — on ADO a work-item write from this stage
// would be the PR-as-work-item hazard, so the tests assert it stays empty.
type adoPRDetailState struct {
	mu           sync.Mutex
	status       string // "completed" | "active" | "abandoned"
	mergeCommit  string
	autoComplete bool
	detailCalls  int
	workItemHits []string
}

func newADOMergeQueuePollServer(t *testing.T, owner, project, name string, st *adoPRDetailState) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	prPath := "/" + owner + "/" + project + "/_apis/git/repositories/" + name + "/pullrequests/9"
	mux.HandleFunc(prPath, func(w http.ResponseWriter, _ *http.Request) {
		st.mu.Lock()
		st.detailCalls++
		body := map[string]interface{}{
			"pullRequestId":   9,
			"status":          st.status,
			"lastMergeCommit": map[string]string{"commitId": st.mergeCommit},
		}
		if st.autoComplete {
			body["autoCompleteSetBy"] = map[string]string{"uniqueName": "goobers"}
		}
		st.mu.Unlock()
		writeFakeJSON(w, body)
	})
	// Catch-all: a work-item touch is the PR-as-work-item hazard this stage
	// must never trigger on ADO; record it so the assertions can prove it did
	// not happen.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/_apis/wit/") {
			st.mu.Lock()
			st.workItemHits = append(st.workItemHits, r.URL.Path)
			st.mu.Unlock()
		}
		writeFakeJSON(w, map[string]interface{}{})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func adoMergeQueuePollEnv(t *testing.T, serverURL, owner, project, name string, grantComplete bool, inputs map[string]string) (root, workDir string) {
	t.Helper()
	root = initDemo(t)
	prev := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = serverURL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-merge-ado-1")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	if grantComplete {
		// The ADO counterpart to github:pr:merge — completion authority.
		t.Setenv("GOOBERS_CRED_ADO_PR_COMPLETE", "test-token")
	}
	t.Setenv(executor.RepoProviderEnvVar, "ado")
	t.Setenv(executor.RepoOwnerEnvVar, owner)
	t.Setenv(executor.RepoProjectEnvVar, project)
	t.Setenv(executor.RepoNameEnvVar, name)
	for k, v := range inputs {
		t.Setenv("GOOBERS_INPUT_"+strings.ToUpper(k), v)
	}
	workDir = t.TempDir()
	t.Chdir(workDir)
	return root, workDir
}

// TestMergeQueuePollADOReportsMergedWithoutBranchCleanupOrWorkItemWrite is the
// CORE ADO land oracle (merge-wiring-plan §1d/§7-step-2): an auto-complete PR
// that ADO reports completed lands as queueOutcome=merged, with the GitHub-only
// branch cleanup and PR-as-work-item remediation both documented no-ops.
func TestMergeQueuePollADOReportsMergedWithoutBranchCleanupOrWorkItemWrite(t *testing.T) {
	st := &adoPRDetailState{status: "completed", mergeCommit: "adomergesha"}
	server := newADOMergeQueuePollServer(t, "acme", "proj", "svc", st)
	root, dir := adoMergeQueuePollEnv(t, server.URL, "acme", "proj", "svc", true, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "merged" {
		t.Fatalf("result = %+v, want queueOutcome=merged", result)
	}
	if result["mergeSha"] != "adomergesha" {
		t.Fatalf("result = %+v, want mergeSha=adomergesha", result)
	}
	if _, ok := result["branchCleanup"]; ok {
		t.Fatalf("result = %+v, want no branchCleanup on ADO — cleanupMergedBranch is a documented no-op (nil HeadRepository)", result)
	}
	if len(st.workItemHits) != 0 {
		t.Fatalf("work-item writes = %v, want none — a merged PR must never mutate a work item that shares its numeric id", st.workItemHits)
	}
}

// TestMergeQueuePollADORequiresCompleteCapability proves completion authority on
// ADO rides on ado:pr:complete (capability.ADOPRComplete), resolved before the
// provider is ever constructed — mirroring how the GitHub path gates on
// github:pr:merge.
func TestMergeQueuePollADORequiresCompleteCapability(t *testing.T) {
	st := &adoPRDetailState{status: "completed", mergeCommit: "adomergesha"}
	server := newADOMergeQueuePollServer(t, "acme", "proj", "svc", st)
	root, _ := adoMergeQueuePollEnv(t, server.URL, "acme", "proj", "svc", false, map[string]string{
		"pullNumber": "9",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 1 || !strings.Contains(stderr, "ado:pr:complete") {
		t.Fatalf("code = %d, stderr = %q, want an ado:pr:complete capability error", code, stderr)
	}
	if st.detailCalls != 0 {
		t.Fatalf("detail calls = %d, want 0 — completion authority must be resolved before the provider polls", st.detailCalls)
	}
}

// TestMergeQueuePollADOEvictionRecordsOutcomeWithoutWorkItemWrite proves the
// eviction remediation labeling (UpdateWorkItem(ID: prNumber, …) on GitHub) is
// gated OFF on ADO: the outcome is written for queue-gate, but no work item is
// touched (PR-as-work-item hazard, merge-wiring-plan §1d/§8).
func TestMergeQueuePollADOEvictionRecordsOutcomeWithoutWorkItemWrite(t *testing.T) {
	// Active with no auto-complete armed → ADO cleared it → Evicted.
	st := &adoPRDetailState{status: "active"}
	server := newADOMergeQueuePollServer(t, "acme", "proj", "svc", st)
	root, dir := adoMergeQueuePollEnv(t, server.URL, "acme", "proj", "svc", true, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != "evicted" {
		t.Fatalf("result = %+v, want queueOutcome=evicted", result)
	}
	if len(st.workItemHits) != 0 {
		t.Fatalf("work-item writes = %v, want none — eviction remediation labeling is gated off on ADO", st.workItemHits)
	}
}

func TestMergeQueuePollRequiresPullNumber(t *testing.T) {
	st := &mergeQueuePollServerState{}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, _ := mergeQueuePollEnv(t, server.URL, map[string]string{})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 1 || !strings.Contains(stderr, "pullNumber") {
		t.Fatalf("code = %d, stderr = %q, want a pullNumber-required error", code, stderr)
	}
}

func TestMergeQueuePollRefusesWithoutCapability(t *testing.T) {
	instanceRoot := initDemo(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: "http://unused.invalid"}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })
	t.Setenv("GOOBERS_RUN_ID", "run-merge-1")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_PULLNUMBER", "9")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "merge-queue-poll", instanceRoot)
	if code != 1 || !strings.Contains(stderr, "github:pr:merge") {
		t.Fatalf("code = %d, stderr = %q, want a github:pr:merge capability error", code, stderr)
	}
}

// TestMergeQueuePollRecordsOptOutDequeueTimeoutForReconciliation covers the
// opt-out path that runs out of time without ever confirming removal.
//
// The watcher fails, correctly — removal was not confirmed. But the entry may
// still be queued and may still merge after the watcher exits, and a merge that
// lands then gets none of the follow-up the normal path performs: branch
// cleanup, issue close-out, sibling fan-out, unparking. Recording the pull
// request for post-merge reconciliation is what lets a later merge still be
// picked up, so the record must be written before the failure is reported.
func TestMergeQueuePollRecordsOptOutDequeueTimeoutForReconciliation(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript:           []string{"pending"},
		pendingCalls:            1_000_000,
		terminalState:           "open",
		labels:                  []string{noMergeReviewLabel},
		optOutAfterGraphQLPolls: 1,
		// Every dequeue attempt fails, so removal is never confirmed and the
		// poll runs to its deadline still believing the entry may be queued.
		dequeueFailures: 1_000_000,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, _ := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "200ms",
	})

	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 — an unconfirmed opt-out dequeue is a failure", code)
	}
	if !strings.Contains(stderr, "could not be confirmed before timeout") {
		t.Fatalf("stderr = %q, want the unconfirmed-removal error", stderr)
	}

	entry := loadPostMergeReconcileEntry(t, root, postMergeTestRepo(), "9")
	if entry.State != postMergeReconcilePending {
		t.Fatalf("post-merge reconcile entry state = %q, want %q — a pull request whose "+
			"dequeue was never confirmed must stay recoverable if it merges later",
			entry.State, postMergeReconcilePending)
	}
	if entry.TimedOutAt.IsZero() {
		t.Fatal("post-merge reconcile entry has no TimedOutAt stamp")
	}
}

func TestMergeQueuePollRefusesUnsupportedGiteaProviderBeforeGitHubDispatch(t *testing.T) {
	root := initDemo(t)
	configureRemediationGitea(t, root, "https://gitea.example.test")
	t.Chdir(t.TempDir())
	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, `does not support repository provider "gitea"`) {
		t.Fatalf("stderr = %q, want explicit unsupported-provider error", stderr)
	}
}
