package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/worktree"
)

// rebasePRServerState is a small stateful fake GitHub server for rebase-pr's
// (#363) tests: just the one PR's label state (GetWorkItem + label add/
// remove), since rebase-pr never lists PRs — its inputs arrive via
// InputsFrom, mirroring the real pr-remediation.yaml wiring.
type rebasePRServerState struct {
	mu     sync.Mutex
	labels []string
}

func (s *rebasePRServerState) start(t *testing.T, owner, repo string, prNumber int) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + owner + "/" + repo
	mux := http.NewServeMux()

	mux.HandleFunc(fmt.Sprintf("%s/issues/%d", prefix, prNumber), func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeFakeJSON(w, map[string]interface{}{
				"number": prNumber, "state": "open", "labels": labelsJSON(s.labels),
				"html_url": fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, prNumber),
			})
		default:
			http.Error(w, fmt.Sprintf("unhandled %s %s", r.Method, r.URL.Path), http.StatusNotImplemented)
		}
	})
	mux.HandleFunc(fmt.Sprintf("%s/issues/%d/labels/", prefix, prNumber), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "want DELETE", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("%s/issues/%d/labels/", prefix, prNumber))
		s.mu.Lock()
		var kept []string
		for _, l := range s.labels {
			if l != name {
				kept = append(kept, l)
			}
		}
		s.labels = kept
		s.mu.Unlock()
		writeFakeJSON(w, []map[string]string{})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// rebasePREnv sets up a runnable rebase-pr CLI invocation against wtPath (the
// worktree gather-pr-context would have checked the PR branch out into).
func rebasePREnv(t *testing.T, serverURL, wtPath string, inputs map[string]string) (instanceRoot string) {
	t.Helper()
	instanceRoot = initDemo(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: serverURL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-363")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	for k, v := range inputs {
		t.Setenv("GOOBERS_INPUT_"+strings.ToUpper(k), v)
	}
	t.Chdir(wtPath)
	return instanceRoot
}

// initNonConflictingPRBranch builds a bare origin (no network) with a PR
// branch that will rebase CLEANLY onto an advanced main: the PR branch and
// main's new commit touch different files.
func initNonConflictingPRBranch(t *testing.T, prBranch string) (origin string) {
	t.Helper()
	origin, _, _ = initPRBranchOrigin(t, prBranch)
	return origin
}

// initConflictingPRBranch builds a bare origin where the PR branch and main
// both modify the SAME line of the SAME file after branching — a real
// rebase conflict, not a synthetic flag.
func initConflictingPRBranch(t *testing.T, prBranch string) (origin string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(work, "shared.txt"), []byte("line one\n"), 0o644); err != nil {
		t.Fatalf("write shared file: %v", err)
	}
	runGitT(t, work, "add", "shared.txt")
	runGitT(t, work, "commit", "-m", "seed")
	runGitT(t, work, "push", "origin", "main")

	runGitT(t, work, "checkout", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(work, "shared.txt"), []byte("line one\nPR's change\n"), 0o644); err != nil {
		t.Fatalf("write PR change: %v", err)
	}
	runGitT(t, work, "commit", "-am", "PR work")
	runGitT(t, work, "push", "origin", prBranch)

	runGitT(t, work, "checkout", "main")
	if err := os.WriteFile(filepath.Join(work, "shared.txt"), []byte("line one\nmain's conflicting change\n"), 0o644); err != nil {
		t.Fatalf("write main's conflicting change: %v", err)
	}
	runGitT(t, work, "commit", "-am", "main moved on, same line")
	runGitT(t, work, "push", "origin", "main")

	return origin
}

// prWorktree provisions the worktree the runner would create for a
// pr-remediation stage — gather-pr-context's own checkoutExistingBranch is
// exercised directly here rather than re-running the full gather-pr-context
// CLI, since rebase-pr's tests are about the rebase decision, not selection.
func prWorktree(t *testing.T, origin, prBranch string) *worktree.Worktree {
	t.Helper()
	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-363-rebase-pr", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-363",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })
	if _, err := checkoutExistingBranch(wt.Path, prBranch, "test-token"); err != nil {
		t.Fatalf("checkoutExistingBranch: %v", err)
	}
	return wt
}

// TestRebasePRCleanNoSubstantiveForcePushesAndClearsLabel is #363's headline
// acceptance for the fast path: a PR whose rebase applies cleanly and whose
// verdict carried no substantive finding gets force-pushed and its
// needs-remediation label cleared, right here — no agentic chain needed.
func TestRebasePRCleanNoSubstantiveForcePushesAndClearsLabel(t *testing.T) {
	const prBranch = "goobers/impl/run-a"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel, "some-other-label"}}
	server := st.start(t, "your-org", "your-repo", 55)

	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "55",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "clean rebase") {
		t.Fatalf("stdout = %q, want a mention of a clean rebase", stdout)
	}

	// The rebase must have actually applied: main's commit (unrelated.txt)
	// should now be an ancestor of the checked-out branch's tip.
	if _, err := os.Stat(filepath.Join(wt.Path, "unrelated.txt")); err != nil {
		t.Fatalf("unrelated.txt (main's commit) missing after rebase: %v", err)
	}

	// The push must have reached origin (force-with-lease), not just the
	// local worktree.
	verify := t.TempDir()
	runGitT(t, verify, "clone", "--branch", prBranch, origin, filepath.Join(verify, "check"))
	if _, err := os.Stat(filepath.Join(verify, "check", "unrelated.txt")); err != nil {
		t.Fatalf("origin's %s branch missing the rebased commit after force-push: %v", prBranch, err)
	}

	st.mu.Lock()
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	for _, l := range labels {
		if l == needsRemediationLabel {
			t.Fatalf("labels = %v, want %s cleared", labels, needsRemediationLabel)
		}
	}
	if len(labels) != 1 || labels[0] != "some-other-label" {
		t.Fatalf("labels = %v, want only the untouched other label to remain", labels)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"false"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=false", data)
	}
}

// TestRebasePRSubstantiveFindingDefersEvenWithCleanRebase proves routing is
// finding-driven, never rebase-driven (design doc §5 D3): a clean rebase
// must NOT suppress a known substantive finding — no push, label untouched.
func TestRebasePRSubstantiveFindingDefersEvenWithCleanRebase(t *testing.T) {
	const prBranch = "goobers/impl/run-b"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 56)

	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "56",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "true",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "needs agentic remediation") {
		t.Fatalf("stdout = %q, want a mention of needing agentic remediation", stdout)
	}

	st.mu.Lock()
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	if len(labels) != 1 || labels[0] != needsRemediationLabel {
		t.Fatalf("labels = %v, want %s left untouched (no push/clear on this path)", labels, needsRemediationLabel)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"true"`) || !strings.Contains(string(data), `"conflict":"false"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true conflict=false", data)
	}
}

func TestRebasePRFailingCIPushesCleanRebaseAndDefersToCheckpoint(t *testing.T) {
	const prBranch = "goobers/impl/run-ci-red"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{}
	server := st.start(t, "your-org", "your-repo", 58)

	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "58",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
		"hasFailingCI":           "true",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"true"`) || !strings.Contains(string(data), `"conflict":"false"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true conflict=false", data)
	}

	verify := t.TempDir()
	runGitT(t, verify, "clone", "--branch", prBranch, origin, filepath.Join(verify, "check"))
	if _, err := os.Stat(filepath.Join(verify, "check", "unrelated.txt")); err != nil {
		t.Fatalf("origin's branch missing clean rebase before checkpoint routing: %v", err)
	}
}

// TestRebasePRConflictDefersAndLeavesCleanWorktree proves a rebase conflict
// is itself treated as substantive (routes to needsAgent) AND that the
// worktree is left in a clean, non-mid-rebase state — never a broken
// conflicted tree for whatever runs next.
func TestRebasePRConflictDefersAndLeavesCleanWorktree(t *testing.T) {
	const prBranch = "goobers/impl/run-c"
	origin := initConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 57)

	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "57",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "needs agentic remediation") {
		t.Fatalf("stdout = %q, want a mention of needing agentic remediation", stdout)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"true"`) || !strings.Contains(string(data), `"conflict":"true"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true conflict=true", data)
	}

	// The worktree must not be mid-rebase (no unmerged/conflicted paths) —
	// attemptRebase must have aborted, or the next stage (or this same
	// worktree, if reused) would inherit a broken tree. rebase-result.json
	// itself is expected to be untracked, so this checks for unmerged paths
	// specifically rather than requiring a fully empty status.
	if unmerged := strings.TrimSpace(runGitOutputT(t, wt.Path, "diff", "--name-only", "--diff-filter=U")); unmerged != "" {
		t.Fatalf("unmerged paths = %q, want none after the aborted rebase", unmerged)
	}
	gitDirCmd := runGitOutputT(t, wt.Path, "rev-parse", "--git-dir")
	gitDir := strings.TrimSpace(gitDirCmd)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wt.Path, gitDir)
	}
	for _, marker := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, marker)); err == nil {
			t.Fatalf("%s exists — a rebase is still in progress, want it aborted", marker)
		}
	}

	// No push should have happened on this path.
	verify := t.TempDir()
	runGitT(t, verify, "clone", "--branch", prBranch, origin, filepath.Join(verify, "check"))
	if _, err := os.Stat(filepath.Join(verify, "check", "unrelated.txt")); err == nil {
		t.Fatal("origin's branch was rebased/pushed, want it untouched on the conflict path")
	}
}

// TestForcePushWithLeaseRefusesOnStaleExpectedSHA is #363's safety-net
// acceptance for design doc §5's "force-with-lease is mandatory" claim, unit
// -tested directly against forcePushWithLease/checkoutExistingBranch: a push
// landing on the SAME branch after checkoutExistingBranch captured its
// fetchedSHA (simulating a human or another process racing rebase-pr
// between its own checkout and its own push) must cause the later
// force-with-lease push to be REFUSED, not silently clobbered. A CLI-level
// version of this race is not deterministically reproducible (rebase-pr's
// own checkoutExistingBranch always re-observes the CURRENT remote tip
// immediately before it would push, so anything that lands strictly before
// that point is correctly absorbed, not raced) — this drives the two
// primitives directly to prove the lease value itself is load-bearing, not
// just present on the command line.
func TestForcePushWithLeaseRefusesOnStaleExpectedSHA(t *testing.T) {
	const prBranch = "goobers/impl/run-e"
	origin := initNonConflictingPRBranch(t, prBranch)

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-363-lease", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-363-lease",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	staleSHA, err := checkoutExistingBranch(wt.Path, prBranch, "test-token")
	if err != nil {
		t.Fatalf("checkoutExistingBranch: %v", err)
	}

	// A push lands on the SAME branch AFTER staleSHA was captured — exactly
	// the race window forcePushWithLease's expectedSHA parameter exists to
	// catch.
	other := t.TempDir()
	runGitT(t, other, "clone", "--branch", prBranch, origin, filepath.Join(other, "human"))
	humanDir := filepath.Join(other, "human")
	runGitT(t, humanDir, "config", "user.name", "human")
	runGitT(t, humanDir, "config", "user.email", "human@example.com")
	if err := os.WriteFile(filepath.Join(humanDir, "human-change.txt"), []byte("a human's concurrent push\n"), 0o644); err != nil {
		t.Fatalf("write human change: %v", err)
	}
	runGitT(t, humanDir, "add", "human-change.txt")
	runGitT(t, humanDir, "commit", "-m", "human's concurrent commit")
	runGitT(t, humanDir, "push", "origin", prBranch)

	// Make an unrelated local commit to push, using the NOW-STALE staleSHA
	// as the lease's expected value — this must be refused.
	if err := os.WriteFile(filepath.Join(wt.Path, "goober-change.txt"), []byte("goober's change\n"), 0o644); err != nil {
		t.Fatalf("write goober change: %v", err)
	}
	runGitT(t, wt.Path, "add", "goober-change.txt")
	runGitT(t, wt.Path, "commit", "-m", "goober's commit, based on the stale view")

	if err := forcePushWithLease(wt.Path, prBranch, staleSHA, "test-token"); err == nil {
		t.Fatal("forcePushWithLease succeeded against a stale expectedSHA — the human's concurrent commit would have been clobbered")
	} else if !strings.Contains(err.Error(), "stale") && !strings.Contains(err.Error(), "rejected") && !strings.Contains(err.Error(), "fetch first") {
		t.Fatalf("forcePushWithLease error = %v, want a lease-rejection error", err)
	}

	// The human's commit must still be on origin, untouched.
	verify := t.TempDir()
	runGitT(t, verify, "clone", "--branch", prBranch, origin, filepath.Join(verify, "check"))
	if _, err := os.Stat(filepath.Join(verify, "check", "human-change.txt")); err != nil {
		t.Fatalf("human-change.txt missing from origin after the refused push — it was clobbered: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verify, "check", "goober-change.txt")); err == nil {
		t.Fatal("goober-change.txt reached origin — the stale-lease push should have been refused entirely")
	}
}

// TestRebasePRRefusesWithoutCapability proves rebase-pr fails closed before
// any git/provider call when a required capability is absent.
func TestRebasePRRefusesWithoutCapability(t *testing.T) {
	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-363-nocap")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	// Deliberately no GOOBERS_CRED_* set.
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "58")
	t.Setenv("GOOBERS_INPUT_HEAD", "goobers/impl/run-d")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1 (fail closed on missing capability)", code, stderr)
	}
}
