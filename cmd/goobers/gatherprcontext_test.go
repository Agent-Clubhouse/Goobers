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

	"github.com/goobers/goobers/internal/worktree"
)

// gatherPRContextServer is a small stateful fake GitHub server for
// gather-pr-context's tests: one open PR, its check state, and a fixed set of
// comments (one of which may carry an embedded verdict-json payload).
type gatherPRContextServer struct {
	owner, repo string
	prNumber    int
	head, base  string
	headSHA     string
	baseSHA     string
	checkState  string
	labels      []string
	comments    []map[string]interface{}
}

func (s gatherPRContextServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + s.owner + "/" + s.repo
	mux := http.NewServeMux()

	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("base"); got != s.base {
			t.Fatalf("ListPullRequests base query = %q, want %q", got, s.base)
		}
		labelObjs := make([]map[string]string, len(s.labels))
		for i, l := range s.labels {
			labelObjs[i] = map[string]string{"name": l}
		}
		writeFakeJSON(w, []map[string]interface{}{
			{
				"number": s.prNumber, "draft": false,
				"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", s.owner, s.repo, s.prNumber),
				"head":     map[string]interface{}{"ref": s.head, "sha": s.headSHA},
				"base":     map[string]interface{}{"ref": s.base, "sha": s.baseSHA},
				"labels":   labelObjs,
			},
		})
	})
	mux.HandleFunc(fmt.Sprintf("%s/commits/%s/status", prefix, s.headSHA), func(w http.ResponseWriter, r *http.Request) {
		state := s.checkState
		if state == "" {
			state = "success"
		}
		writeFakeJSON(w, map[string]interface{}{
			"state": state,
			"statuses": []map[string]interface{}{
				{"context": "ci", "state": state},
			},
		})
	})
	mux.HandleFunc(fmt.Sprintf("%s/commits/%s/check-runs", prefix, s.headSHA), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"check_runs": []map[string]interface{}{}})
	})
	mux.HandleFunc(fmt.Sprintf("%s/issues/%d/comments", prefix, s.prNumber), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, s.comments)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// initPRBranchOrigin builds a local bare origin (#237's no-network pattern)
// seeded with a "main" commit, an existing PR branch cut from that seed
// carrying one further commit, and THEN advances main past the point the PR
// branched from — so the PR is genuinely behind, giving
// TestGatherPRContextChecksOutSelectedPRAndLoadsContext something real to
// detect. Returns the bare origin path plus the PR head SHA and main's new
// (post-advance) tip SHA.
func initPRBranchOrigin(t *testing.T, prBranch string) (origin, headSHA, baseSHA string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGitT(t, work, "add", "README.md")
	runGitT(t, work, "commit", "-m", "seed")
	runGitT(t, work, "push", "origin", "main")

	runGitT(t, work, "checkout", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("pr work\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	runGitT(t, work, "add", "feature.txt")
	runGitT(t, work, "commit", "-m", "pr work")
	runGitT(t, work, "push", "origin", prBranch)
	headSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	runGitT(t, work, "checkout", "main")
	if err := os.WriteFile(filepath.Join(work, "unrelated.txt"), []byte("main moved on\n"), 0o644); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
	runGitT(t, work, "add", "unrelated.txt")
	runGitT(t, work, "commit", "-m", "main moved on")
	runGitT(t, work, "push", "origin", "main")
	baseSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	return origin, headSHA, baseSHA
}

// TestGatherPRContextChecksOutSelectedPRAndLoadsContext is #362's headline
// acceptance: one open PR labeled needs-remediation gets selected, its
// branch is checked out into the run's worktree (replacing the runner's own
// default branch), the base-advanced-since-branching state is detected, and
// the latest embedded verdict + full comment thread are loaded.
func TestGatherPRContextChecksOutSelectedPRAndLoadsContext(t *testing.T) {
	const prBranch = "goobers/impl/run-a"
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	verdictComment := "**merge-review verdict: needs-changes**\n\nRebase and address one nit.\n\n" +
		`<!-- verdict-json: {"decision":"needs-changes","summary":"Rebase and address one nit.","findings":[{"severity":"warning","message":"nit","class":"substantive"}],"headSha":"` + headSHA + `","baseSha":"` + baseSHA + `"} -->`

	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 55, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{"goobers:needs-remediation"},
		comments: []map[string]interface{}{
			{"id": 1, "user": map[string]string{"login": "human-reviewer"}, "body": "please rebase", "created_at": "2026-07-01T00:00:00Z"},
			{"id": 2, "user": map[string]string{"login": "merge-review-bot"}, "body": verdictComment, "created_at": "2026-07-02T00:00:00Z"},
		},
	}
	server := srv.start(t)

	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-362", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-362",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-362")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "PR #55") {
		t.Fatalf("stdout = %q, want a mention of PR #55", stdout)
	}

	branch := strings.TrimSpace(runGitOutputT(t, wt.Path, "symbolic-ref", "--short", "HEAD"))
	if branch != prBranch {
		t.Fatalf("checked-out branch = %q, want %q (the PR's own branch, not the runner's default)", branch, prBranch)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "pr-context.json"))
	if err != nil {
		t.Fatalf("read pr-context.json: %v", err)
	}
	var got struct {
		SelectedNumber         string `json:"selectedNumber"`
		Head                   string `json:"head"`
		IsBehindBase           bool   `json:"isBehindBase"`
		HasSubstantiveFindings string `json:"hasSubstantiveFindings"`
		Verdict                struct {
			Decision string `json:"decision"`
			Findings []struct {
				Class string `json:"class"`
			} `json:"findings"`
		} `json:"verdict"`
		Comments []struct {
			Author string `json:"author"`
			Body   string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal pr-context.json: %v (data=%s)", err, data)
	}
	if got.SelectedNumber != "55" || got.Head != prBranch {
		t.Fatalf("got = %+v, want selectedNumber=\"55\" head=%q", got, prBranch)
	}
	if !got.IsBehindBase {
		t.Fatal("isBehindBase = false, want true — main advanced past the PR's branch point")
	}
	if got.Verdict.Decision != "needs-changes" || len(got.Verdict.Findings) != 1 || got.Verdict.Findings[0].Class != "substantive" {
		t.Fatalf("verdict = %+v, want the embedded needs-changes verdict recovered from the comment thread", got.Verdict)
	}
	if got.HasSubstantiveFindings != "true" {
		t.Fatalf("hasSubstantiveFindings = %q, want \"true\" (the embedded verdict has a substantive finding)", got.HasSubstantiveFindings)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("comments = %+v, want both thread comments surfaced", got.Comments)
	}
}

func TestGatherPRContextSelectsUnlabeledFailingPR(t *testing.T) {
	const prBranch = "goobers/impl/run-ci-red"
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 56, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA,
		checkState: "failure",
	}
	server := srv.start(t)

	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-ci-red", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-ci-red",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-ci-red")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "PR #56") {
		t.Fatalf("stdout = %q, want a mention of PR #56", stdout)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "pr-context.json"))
	if err != nil {
		t.Fatalf("read pr-context.json: %v", err)
	}
	var got struct {
		SelectedNumber string `json:"selectedNumber"`
		Head           string `json:"head"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal pr-context.json: %v (data=%s)", err, data)
	}
	if got.SelectedNumber != "56" || got.Head != prBranch {
		t.Fatalf("got = %+v, want selectedNumber=\"56\" head=%q", got, prBranch)
	}
}

// TestGatherPRContextNoEligiblePRIsNoWork proves gather-pr-context succeeds
// (exit 0, no-work) rather than erroring when no PR is labeled or failing —
// a normal outcome (mirrors pr-select's own no-work shape), not an error.
func TestGatherPRContextNoEligiblePRIsNoWork(t *testing.T) {
	srv := gatherPRContextServer{owner: "your-org", repo: "your-repo", base: "main"}
	server := srv.start(t)

	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-362-empty")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q, want 0 (no-work)", code, stderr)
	}
}

// TestGatherPRContextRefusesWithoutCapability proves gather-pr-context fails
// closed before any provider/git call when a required capability is absent.
func TestGatherPRContextRefusesWithoutCapability(t *testing.T) {
	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-362-nocap")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	// Deliberately no GOOBERS_CRED_* set.
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1 (fail closed on missing capability)", code, stderr)
	}
}
