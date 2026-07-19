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

	"github.com/goobers/goobers/internal/executor"
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

	pollCalls     int
	graphqlPolls  int
	pullListCalls int
	deleteCalls   int
	labelCalls    int
	commentCalls  int
	labelStatus   int // non-zero forces the labels endpoint to fail
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
		st.mu.Lock()
		st.graphqlPolls++
		terminal := st.graphqlPolls > st.pendingCalls
		terminalState, terminalMerged, entryAbsent := st.terminalState, st.terminalMerged, st.pendingEntryAbsent
		st.mu.Unlock()

		pr := map[string]interface{}{"state": "OPEN", "merged": false, "mergeCommit": nil}
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

// TestMergeQueuePollToleratesAnAbsentEntryBeforeItHasSeenOne guards the
// other side of #885: an absent entry is ALSO what the gap between
// merge-pr's enqueue and the entry becoming visible looks like. Reading
// that as an eviction would label a perfectly healthy just-enqueued pull
// request needs-remediation. Before any entry has been seen, absence is
// tolerated for a grace window — so a poll whose budget expires inside that
// window reports timeout, never eviction.
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
	if st.labelCalls != 0 {
		t.Fatalf("label calls = %d, want 0 — a just-enqueued pull request must never be labeled needs-remediation", st.labelCalls)
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
// queueOutcome=timeout, distinct from both merged and evicted, with exit 0
// (still pending is not itself a stage failure).
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
	if st.labelCalls != 0 || st.deleteCalls != 0 {
		t.Fatalf("label/delete calls = %d/%d, want 0/0 for a timeout (no terminal outcome to act on yet)", st.labelCalls, st.deleteCalls)
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
			budget := mergeQueuePollBudget(tc.stage)
			if budget <= 0 {
				t.Fatalf("budget = %s, want a positive poll budget", budget)
			}
			if budget >= tc.stage {
				t.Fatalf("budget = %s, want strictly less than the %s stage deadline — a poll that outlasts its stage is SIGKILLed before it can write a result", budget, tc.stage)
			}
		})
	}
	// The specific pairing that caused the live failure.
	if got := mergeQueuePollBudget(executor.DefaultTimeout); got >= executor.DefaultPollTimeout {
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
	if budget := mergeQueuePollBudget(stageTimeout()); budget <= executor.DefaultTimeout {
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
		"pollTimeoutSeconds": "30m",
		"timeout":            "80ms",
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
