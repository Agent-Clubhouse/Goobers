package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

const portalBuildMakeEnv = "GOOBERS_TEST_PORTAL_BUILD_MAKE"

// rebasePRServerState is a small stateful fake GitHub server for rebase-pr's
// (#363) tests: one PR's label state and durable handoff comments. rebase-pr
// never lists PRs — its core inputs arrive via InputsFrom, mirroring the real
// pr-remediation.yaml wiring.
type rebasePRServerState struct {
	mu       sync.Mutex
	labels   []string
	comments []string
	// commentAuthors and commentCreatedAt are optional, index-aligned with
	// comments: an empty (or short) entry falls back to the bot login /
	// default timestamp, so existing tests that only set comments are
	// unaffected. They let the human-comment detection tests vary a comment's
	// author and created-at independently.
	commentAuthors      []string
	commentCreatedAt    []string
	rejectCommentUpdate bool
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
	mux.HandleFunc(fmt.Sprintf("%s/issues/%d/comments", prefix, prNumber), func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := make([]map[string]interface{}, len(s.comments))
		for i, comment := range s.comments {
			login := "goobers-bot"
			if i < len(s.commentAuthors) && s.commentAuthors[i] != "" {
				login = s.commentAuthors[i]
			}
			createdAt := "2026-07-15T00:00:00Z"
			if i < len(s.commentCreatedAt) && s.commentCreatedAt[i] != "" {
				createdAt = s.commentCreatedAt[i]
			}
			out[i] = map[string]interface{}{
				"id": i + 1, "user": map[string]string{"login": login},
				"body": comment, "created_at": createdAt,
			}
		}
		writeFakeJSON(w, out)
	})
	mux.HandleFunc(prefix+"/issues/comments/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "want PATCH", http.StatusMethodNotAllowed)
			return
		}
		if s.rejectCommentUpdate {
			http.Error(w, "comment update rejected by test", http.StatusInternalServerError)
			return
		}
		id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, prefix+"/issues/comments/"))
		if err != nil {
			http.Error(w, "invalid comment id", http.StatusBadRequest)
			return
		}
		var req struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if id < 1 || id > len(s.comments) {
			http.NotFound(w, r)
			return
		}
		s.comments[id-1] = req.Body
		writeFakeJSON(w, map[string]interface{}{
			"id": id, "user": map[string]string{"login": "goobers-bot"}, "body": req.Body,
		})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]string{"login": "goobers-bot"})
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
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_REPO_PROVIDER", "github")
	t.Setenv("GOOBERS_REPO_OWNER", "your-org")
	t.Setenv("GOOBERS_REPO_NAME", "your-repo")
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

func initSharedFoundationPRBranches(t *testing.T, prBranch string) (origin string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	runGitT(t, work, "add", "README.md")
	runGitT(t, work, "commit", "-m", "seed")
	runGitT(t, work, "push", "origin", "main")

	const foundationBranch = "goobers/impl/foundation"
	runGitT(t, work, "checkout", "-b", foundationBranch)
	if err := os.WriteFile(filepath.Join(work, "foundation.txt"), []byte("shared foundation\n"), 0o644); err != nil {
		t.Fatalf("write foundation: %v", err)
	}
	runGitT(t, work, "add", "foundation.txt")
	runGitT(t, work, "commit", "-m", "shared foundation")
	foundationSHA := strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))
	runGitT(t, work, "push", "origin", foundationBranch)

	runGitT(t, work, "checkout", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(work, "unique.txt"), []byte("unique PR work\n"), 0o644); err != nil {
		t.Fatalf("write unique PR work: %v", err)
	}
	runGitT(t, work, "add", "unique.txt")
	runGitT(t, work, "commit", "-m", "unique PR work")
	runGitT(t, work, "push", "origin", prBranch)

	runGitT(t, work, "checkout", "main")
	runGitT(t, work, "merge", "--squash", foundationBranch)
	runGitT(t, work, "commit", "-m", "land foundation through separate PR")
	if landedSHA := strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD")); landedSHA == foundationSHA {
		t.Fatalf("squash merge retained foundation SHA %q, want a distinct commit identity", foundationSHA)
	}
	runGitT(t, work, "push", "origin", "main")
	return origin
}

func initAdjacentAdditionPRBranch(t *testing.T, prBranch string) (origin string) {
	t.Helper()
	return initAttributedAdjacentAdditionPRBranch(t, prBranch, "")
}

func initAttributedAdjacentAdditionPRBranch(t *testing.T, prBranch, attributes string) (origin string) {
	t.Helper()
	return initAdjacentConflictPRBranch(
		t,
		prBranch,
		"items.yaml",
		"items:\n  - existing\n",
		"items:\n  - existing\n  - from-pr\n",
		"items:\n  - existing\n  - from-base\n",
		attributes,
	)
}

func initAdjacentConflictPRBranch(t *testing.T, prBranch, name, ancestor, incoming, upstream, attributes string) (origin string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(work, name), []byte(ancestor), 0o644); err != nil {
		t.Fatalf("write seed conflict file: %v", err)
	}
	if attributes != "" {
		if err := os.WriteFile(filepath.Join(work, ".gitattributes"), []byte(attributes), 0o644); err != nil {
			t.Fatalf("write attributes: %v", err)
		}
		runGitT(t, work, "--literal-pathspecs", "add", "--", name, ".gitattributes")
	} else {
		runGitT(t, work, "--literal-pathspecs", "add", "--", name)
	}
	runGitT(t, work, "commit", "-m", "seed")
	runGitT(t, work, "push", "origin", "main")

	runGitT(t, work, "checkout", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(work, name), []byte(incoming), 0o644); err != nil {
		t.Fatalf("write PR conflict addition: %v", err)
	}
	runGitT(t, work, "commit", "-am", "add PR item")
	runGitT(t, work, "push", "origin", prBranch)

	runGitT(t, work, "checkout", "main")
	if err := os.WriteFile(filepath.Join(work, name), []byte(upstream), 0o644); err != nil {
		t.Fatalf("write base conflict addition: %v", err)
	}
	runGitT(t, work, "commit", "-am", "add base item")
	runGitT(t, work, "push", "origin", "main")

	return origin
}

func initPortalDistConflictPRBranch(t *testing.T, prBranch string, sourceConflict bool) (origin string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	if err := os.MkdirAll(filepath.Join(work, "portal", "src"), 0o755); err != nil {
		t.Fatalf("create portal source directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(work, portalDistPath), 0o755); err != nil {
		t.Fatalf("create portal bundle directory: %v", err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("Makefile", "portal-build:\n\trm -rf cmd/goobers/portal-dist\n\tmkdir -p cmd/goobers/portal-dist\n\tprintf '%s+%s\\n' \"$$(cat portal/src/base.txt)\" \"$$(cat portal/src/app.txt)\" > cmd/goobers/portal-dist/index.html\n")
	write("portal/src/app.txt", "ancestor\n")
	write("portal/src/base.txt", "ancestor\n")
	write(portalDistPath+"/index.html", "ancestor\n")
	runGitT(t, work, "add", ".")
	runGitT(t, work, "commit", "-m", "seed")
	runGitT(t, work, "push", "origin", "main")

	runGitT(t, work, "checkout", "-b", prBranch)
	write("portal/src/app.txt", "from-pr\n")
	write(portalDistPath+"/index.html", "pr-bundle\n")
	runGitT(t, work, "commit", "-am", "change portal in PR")
	runGitT(t, work, "push", "origin", prBranch)

	runGitT(t, work, "checkout", "main")
	if sourceConflict {
		write("portal/src/app.txt", "from-base\n")
	} else {
		write("portal/src/base.txt", "from-base\n")
	}
	write(portalDistPath+"/index.html", "base-bundle\n")
	runGitT(t, work, "commit", "-am", "change portal on base")
	runGitT(t, work, "push", "origin", "main")

	return origin
}

// installPortalBuildMake installs a copy of this test binary as the "make"
// fixture (see installMakeExecutableFixture for why it's a copy, not a hard
// link to the running test binary).
func installPortalBuildMake(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name := "make"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	installMakeExecutableFixture(t, dir, name)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(portalBuildMakeEnv, "1")
}

func runPortalBuildMake() int {
	if len(os.Args) != 2 || os.Args[1] != "portal-build" {
		fmt.Fprintf(os.Stderr, "make fixture: args = %q, want [portal-build]\n", os.Args[1:])
		return 2
	}
	base, err := os.ReadFile(filepath.Join("portal", "src", "base.txt"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "make fixture: read base source: %v\n", err)
		return 1
	}
	app, err := os.ReadFile(filepath.Join("portal", "src", "app.txt"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "make fixture: read app source: %v\n", err)
		return 1
	}
	if err := os.RemoveAll(portalDistPath); err != nil {
		fmt.Fprintf(os.Stderr, "make fixture: remove portal bundle: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(portalDistPath, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "make fixture: create portal bundle: %v\n", err)
		return 1
	}
	data := fmt.Appendf(nil, "%s+%s\n", strings.TrimSpace(string(base)), strings.TrimSpace(string(app)))
	if err := os.WriteFile(filepath.Join(portalDistPath, "index.html"), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "make fixture: write portal bundle: %v\n", err)
		return 1
	}
	return 0
}

// initConflictingPRBranch builds a bare origin where the PR branch and main
// both modify the SAME line inside the SAME Go function after branching — a
// real same-function rebase conflict, not a synthetic flag.
func initConflictingPRBranch(t *testing.T, prBranch string) (origin string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(work, "status.go"), []byte("package status\n\nfunc runStatus() string {\n\treturn \"seed\"\n}\n"), 0o644); err != nil {
		t.Fatalf("write status file: %v", err)
	}
	runGitT(t, work, "add", "status.go")
	runGitT(t, work, "commit", "-m", "seed")
	runGitT(t, work, "push", "origin", "main")

	runGitT(t, work, "checkout", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(work, "status.go"), []byte("package status\n\nfunc runStatus() string {\n\treturn \"pr\"\n}\n"), 0o644); err != nil {
		t.Fatalf("write PR change: %v", err)
	}
	runGitT(t, work, "commit", "-am", "PR work")
	runGitT(t, work, "push", "origin", prBranch)

	runGitT(t, work, "checkout", "main")
	if err := os.WriteFile(filepath.Join(work, "status.go"), []byte("package status\n\nfunc runStatus() string {\n\treturn \"main\"\n}\n"), 0o644); err != nil {
		t.Fatalf("write main's conflicting change: %v", err)
	}
	runGitT(t, work, "commit", "-am", "main moved on, same line")
	runGitT(t, work, "push", "origin", "main")

	return origin
}

func initAdjacentThenStructuralConflictPRBranch(t *testing.T, prBranch string) (origin string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(work, "items.yaml"), []byte("items:\n  - existing\n"), 0o644); err != nil {
		t.Fatalf("write seed list: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "status.go"), []byte("package status\n\nfunc runStatus() string {\n\treturn \"seed\"\n}\n"), 0o644); err != nil {
		t.Fatalf("write seed status: %v", err)
	}
	runGitT(t, work, "add", "items.yaml", "status.go")
	runGitT(t, work, "commit", "-m", "seed")
	runGitT(t, work, "push", "origin", "main")

	runGitT(t, work, "checkout", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(work, "items.yaml"), []byte("items:\n  - existing\n  - from-pr\n"), 0o644); err != nil {
		t.Fatalf("write PR list addition: %v", err)
	}
	runGitT(t, work, "commit", "-am", "add PR item")
	if err := os.WriteFile(filepath.Join(work, "status.go"), []byte("package status\n\nfunc runStatus() string {\n\treturn \"pr\"\n}\n"), 0o644); err != nil {
		t.Fatalf("write PR status change: %v", err)
	}
	runGitT(t, work, "commit", "-am", "change PR status")
	runGitT(t, work, "push", "origin", prBranch)

	runGitT(t, work, "checkout", "main")
	if err := os.WriteFile(filepath.Join(work, "items.yaml"), []byte("items:\n  - existing\n  - from-base\n"), 0o644); err != nil {
		t.Fatalf("write base list addition: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "status.go"), []byte("package status\n\nfunc runStatus() string {\n\treturn \"main\"\n}\n"), 0o644); err != nil {
		t.Fatalf("write base status change: %v", err)
	}
	runGitT(t, work, "commit", "-am", "advance base")
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

func TestRebasePRDropsFoundationLandedThroughSeparatePR(t *testing.T) {
	const prBranch = "goobers/impl/dependent"
	origin := initSharedFoundationPRBranches(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	runGitT(t, wt.Path, "config", "rebase.reapplyCherryPicks", "true")

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 61)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "61",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(wt.Path, "rebase-result.json"))
	if result["needsAgent"] != "false" || result["conflict"] != "false" {
		t.Fatalf("rebase-result.json = %#v, want already-landed foundation handled without escalation", result)
	}

	verify := filepath.Join(t.TempDir(), "check")
	runGitT(t, filepath.Dir(verify), "clone", "--branch", prBranch, origin, verify)
	if got := strings.TrimSpace(runGitOutputT(t, verify, "rev-list", "--count", "origin/main..HEAD")); got != "1" {
		t.Fatalf("unique commit count = %s, want 1 after dropping the shared foundation", got)
	}
	if got := strings.TrimSpace(runGitOutputT(t, verify, "log", "--format=%s", "origin/main..HEAD")); got != "unique PR work" {
		t.Fatalf("unique commits = %q, want only the dependent PR's work", got)
	}
	for _, name := range []string{"foundation.txt", "unique.txt"} {
		if _, err := os.Stat(filepath.Join(verify, name)); err != nil {
			t.Fatalf("%s missing from rebuilt PR branch: %v", name, err)
		}
	}
}

func TestRebaseFetchHeadArgsFallsBackWhenOptionIsUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX git shim to emulate Git 2.17 help")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	script := `#!/bin/sh
if [ "$1" = "rebase" ] && [ "$2" = "-h" ]; then
	echo "usage: git rebase [-i] [options] [--exec <cmd>] [--onto <newbase>] [<upstream> [<branch>]]"
	exit 129
fi
exec "$GOOBERS_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("GOOBERS_TEST_REAL_GIT", realGit)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := rebaseFetchHeadArgs(t.TempDir())
	want := []string{"rebase", "FETCH_HEAD"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("rebaseFetchHeadArgs() = %q, want %q", got, want)
	}
}

func TestRebasePRProviderDeadlineIncludesGitWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX pre-receive hook to delay git")
	}
	const prBranch = "goobers/impl/run-deadline"
	origin := initNonConflictingPRBranch(t, prBranch)
	hook := filepath.Join(origin, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 0.2\n"), 0o755); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 59)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "59",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
		"timeout":                "100ms",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q, want provider deadline failure after Git work consumed the stage budget", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "deadline exceeded") {
		t.Fatalf("stderr = %q, want deadline exceeded", stderr)
	}

	st.mu.Lock()
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	if len(labels) != 1 || labels[0] != needsRemediationLabel {
		t.Fatalf("labels = %v, want %s unchanged after the stage budget expired", labels, needsRemediationLabel)
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
	if !strings.Contains(string(data), `"needsAgent":"true"`) ||
		!strings.Contains(string(data), `"conflict":"false"`) ||
		!strings.Contains(string(data), `"remediationCauses":"substantive"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true conflict=false cause=substantive", data)
	}
}

// TestEvaluateRemediatePolicy is #941/PRR-6's unit-level coverage of the pure
// policy decision: restrictive vs liberal policies, and an unlisted (policy-
// excluded) cause.
func TestEvaluateRemediatePolicy(t *testing.T) {
	tests := []struct {
		name               string
		remediate          string
		conflict           bool
		substantive        bool
		failingCI          bool
		siblingOverlap     bool
		humanComment       bool
		wantNeedsAgent     bool
		wantPolicyExcluded bool
		wantReasonContains string
		wantCauses         string
	}{
		{
			name:           "nothing detected, liberal policy",
			remediate:      defaultRemediatePolicy,
			wantNeedsAgent: false,
		},
		{
			name:           "conflict detected, liberal policy fires",
			remediate:      defaultRemediatePolicy,
			conflict:       true,
			wantNeedsAgent: true,
			wantCauses:     "conflict",
		},
		{
			name:           "restrictive policy: conflict only, conflict detected fires",
			remediate:      "conflict",
			conflict:       true,
			wantNeedsAgent: true,
			wantCauses:     "conflict",
		},
		{
			name:               "restrictive policy: conflict only, substantive detected excluded",
			remediate:          "conflict",
			substantive:        true,
			wantNeedsAgent:     true,
			wantPolicyExcluded: true,
			wantReasonContains: "substantive",
		},
		{
			name:               "restrictive policy excludes an unlisted cause (failing-ci)",
			remediate:          "conflict,substantive",
			failingCI:          true,
			wantNeedsAgent:     true,
			wantPolicyExcluded: true,
			wantReasonContains: "failing-ci",
		},
		{
			name:               "policy excludes multiple detected causes at once",
			remediate:          "behind-base",
			substantive:        true,
			failingCI:          true,
			wantNeedsAgent:     true,
			wantPolicyExcluded: true,
			wantReasonContains: "substantive, failing-ci",
		},
		{
			name:           "one firing cause outweighs an excluded one",
			remediate:      "conflict",
			conflict:       true,
			substantive:    true,
			wantNeedsAgent: true,
			// conflict fires, so this is NOT policyExcluded even though
			// substantive alone would have been excluded.
			wantPolicyExcluded: false,
			wantCauses:         "conflict",
		},
		{
			name:           "sibling overlap detected, liberal policy fires",
			remediate:      defaultRemediatePolicy,
			siblingOverlap: true,
			wantNeedsAgent: true,
			wantCauses:     "sibling-overlap",
		},
		{
			name:           "sibling overlap preserves an independent substantive signal",
			remediate:      defaultRemediatePolicy,
			substantive:    true,
			siblingOverlap: true,
			wantNeedsAgent: true,
			wantCauses:     "substantive,sibling-overlap",
		},
		{
			name:               "empty policy excludes everything detected",
			remediate:          "",
			conflict:           true,
			wantNeedsAgent:     true,
			wantPolicyExcluded: true,
			wantReasonContains: "conflict",
		},
		{
			name:           "human comment fires alone under the new liberal default",
			remediate:      defaultRemediatePolicy,
			humanComment:   true,
			wantNeedsAgent: true,
			wantCauses:     "human-comment",
		},
		{
			name:               "old pinned five-cause policy excludes a detected human comment",
			remediate:          "conflict,substantive,failing-ci,behind-base,sibling-overlap",
			humanComment:       true,
			wantNeedsAgent:     true,
			wantPolicyExcluded: true,
			wantReasonContains: "human-comment",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evaluateRemediatePolicy(test.remediate, test.conflict, test.substantive, test.failingCI, test.siblingOverlap, test.humanComment)
			if got.needsAgent != test.wantNeedsAgent {
				t.Fatalf("needsAgent = %v, want %v", got.needsAgent, test.wantNeedsAgent)
			}
			if got.policyExcluded != test.wantPolicyExcluded {
				t.Fatalf("policyExcluded = %v, want %v", got.policyExcluded, test.wantPolicyExcluded)
			}
			if test.wantReasonContains != "" && !strings.Contains(got.excludedReason, test.wantReasonContains) {
				t.Fatalf("excludedReason = %q, want it to contain %q", got.excludedReason, test.wantReasonContains)
			}
			if got.policyExcluded != got.policyResult.excluded || got.excludedReason != got.policyResult.reason {
				t.Fatalf("policyResult %+v does not mirror policyExcluded/excludedReason", got.policyResult)
			}
			if causes := formatRemediationCauses(got.policyResult.causes); causes != test.wantCauses {
				t.Fatalf("remediation causes = %q, want %q", causes, test.wantCauses)
			}
		})
	}
}

// TestHasNewHumanCommentSince is the pure-table coverage of the human-comment
// detection predicate: which issue-level comment retriggers remediation, given
// the watermark the last checkpoint recorded.
func TestHasNewHumanCommentSince(t *testing.T) {
	const bot = "goobers-bot"
	ptr := func(tm time.Time) *time.Time { return &tm }
	wm := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	after := wm.Add(time.Hour)
	before := wm.Add(-time.Hour)

	stateWith := func(s remediationState) providers.Comment {
		body, err := remediationStateComment(s)
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		// The bot's own machine-payload comment, timestamped after the
		// watermark — it must never itself count as a human comment.
		return providers.Comment{ID: "state", Author: bot, Body: body, CreatedAt: ptr(after)}
	}
	watermarked := stateWith(remediationState{Cycles: 1, LastSeenCommentAt: wm.Format(time.RFC3339)})
	verdict := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictPass})

	tests := []struct {
		name     string
		comments []providers.Comment
		want     bool
	}{
		{
			name: "human comment strictly after the watermark",
			comments: []providers.Comment{
				watermarked,
				{ID: "c", Author: "alice", Body: "please tweak this", CreatedAt: ptr(after)},
			},
			want: true,
		},
		{
			name: "human comment exactly at the watermark does not retrigger",
			comments: []providers.Comment{
				watermarked,
				{ID: "c", Author: "alice", Body: "old", CreatedAt: ptr(wm)},
			},
			want: false,
		},
		{
			name: "human comment before the watermark does not retrigger",
			comments: []providers.Comment{
				watermarked,
				{ID: "c", Author: "alice", Body: "older", CreatedAt: ptr(before)},
			},
			want: false,
		},
		{
			name: "bot-typed author is ignored even after the watermark",
			comments: []providers.Comment{
				watermarked,
				{ID: "c", Author: "some-app", AuthorType: "Bot", Body: "ci", CreatedAt: ptr(after)},
			},
			want: false,
		},
		{
			name: "the authenticated bot itself is ignored",
			comments: []providers.Comment{
				watermarked,
				{ID: "c", Author: bot, Body: "housekeeping", CreatedAt: ptr(after)},
			},
			want: false,
		},
		{
			name: "state present but no watermark fails closed",
			comments: []providers.Comment{
				stateWith(remediationState{Cycles: 1}),
				{ID: "c", Author: "alice", Body: "new", CreatedAt: ptr(after)},
			},
			want: false,
		},
		{
			name: "corrupt watermark fails closed",
			comments: []providers.Comment{
				stateWith(remediationState{Cycles: 1, LastSeenCommentAt: "not-a-timestamp"}),
				{ID: "c", Author: "alice", Body: "new", CreatedAt: ptr(after)},
			},
			want: false,
		},
		{
			name: "no state ever recorded fails open on any human comment",
			comments: []providers.Comment{
				{ID: "c", Author: "alice", Body: "please look", CreatedAt: ptr(before)},
			},
			want: true,
		},
		{
			name: "nil CreatedAt never counts",
			comments: []providers.Comment{
				watermarked,
				{ID: "c", Author: "alice", Body: "no timestamp", CreatedAt: nil},
			},
			want: false,
		},
		{
			name: "machine-payload body never counts even from a human author after the watermark",
			comments: []providers.Comment{
				watermarked,
				{ID: "c", Author: "alice", Body: verdict, CreatedAt: ptr(after)},
			},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasNewHumanCommentSince(test.comments, bot); got != test.want {
				t.Fatalf("hasNewHumanCommentSince = %v, want %v", got, test.want)
			}
		})
	}
}

// TestRebasePRNewHumanCommentDefersToCheckpoint mirrors
// TestRebasePRFailingCIPushesCleanRebaseAndDefersToCheckpoint: a clean rebase
// with no findings and green CI, but a new human comment postdating the sticky
// state's watermark, force-pushes the clean rebase and routes to the checkpoint
// with remediationCauses=human-comment — without clearing needs-remediation.
func TestRebasePRNewHumanCommentDefersToCheckpoint(t *testing.T) {
	const prBranch = "goobers/impl/run-human-comment"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	watermark := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	priorState, err := remediationStateComment(remediationState{
		Cycles: 1, LastDiffDigest: "sha256:prior", LastSeenCommentAt: watermark.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	newer := watermark.Add(time.Hour).Format(time.RFC3339)
	st := &rebasePRServerState{
		labels:           []string{needsRemediationLabel},
		comments:         []string{priorState, "please rename this function"},
		commentAuthors:   []string{"goobers-bot", "alice"},
		commentCreatedAt: []string{watermark.Format(time.RFC3339), newer},
	}
	server := st.start(t, "your-org", "your-repo", 59)

	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "59",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
		"hasFailingCI":           "false",
		"remediate":              defaultRemediatePolicy,
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"true"`) ||
		!strings.Contains(string(data), `"conflict":"false"`) ||
		!strings.Contains(string(data), `"remediationCauses":"human-comment"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true conflict=false cause=human-comment", data)
	}

	st.mu.Lock()
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	if len(labels) != 1 || labels[0] != needsRemediationLabel {
		t.Fatalf("labels = %v, want %s left in place (routed to checkpoint, not cleared)", labels, needsRemediationLabel)
	}

	// The clean rebase must have been force-pushed so CI re-runs on it.
	verify := t.TempDir()
	runGitT(t, verify, "clone", "--branch", prBranch, origin, filepath.Join(verify, "check"))
	if _, err := os.Stat(filepath.Join(verify, "check", "unrelated.txt")); err != nil {
		t.Fatalf("origin's branch missing clean rebase before checkpoint routing: %v", err)
	}
}

// TestRebasePROldPolicyIgnoresHumanComment is the parking-regression guard: the
// same state and new human comment, but a policy pinned to the old five causes,
// must NOT detect the comment — the PR is a clean green rebase, so it is
// force-pushed and needs-remediation is cleared, exactly as before this cause.
func TestRebasePROldPolicyIgnoresHumanComment(t *testing.T) {
	const prBranch = "goobers/impl/run-human-comment-old-policy"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	watermark := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	priorState, err := remediationStateComment(remediationState{
		Cycles: 1, LastDiffDigest: "sha256:prior", LastSeenCommentAt: watermark.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	newer := watermark.Add(time.Hour).Format(time.RFC3339)
	st := &rebasePRServerState{
		labels:           []string{needsRemediationLabel},
		comments:         []string{priorState, "please rename this function"},
		commentAuthors:   []string{"goobers-bot", "alice"},
		commentCreatedAt: []string{watermark.Format(time.RFC3339), newer},
	}
	server := st.start(t, "your-org", "your-repo", 60)

	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "60",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
		"hasFailingCI":           "false",
		"remediate":              "conflict,substantive,failing-ci,behind-base,sibling-overlap",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"false"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=false under the old pinned policy", data)
	}

	st.mu.Lock()
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	if len(labels) != 0 {
		t.Fatalf("labels = %v, want %s cleared on a clean green rebase the old policy leaves untouched", labels, needsRemediationLabel)
	}
}

// TestRebasePRRestrictivePolicyEscalatesUntouchedWithoutForcePush is #941/
// PRR-6's end-to-end acceptance criterion: `remediate: "conflict"` attempts
// conflicts only — a detected substantive finding with no conflict escalates
// untouched (no force-push, label left in place, policyExcluded recorded)
// rather than either silently force-pushing (dropping the finding) or
// invoking the agentic chain.
func TestRebasePRRestrictivePolicyEscalatesUntouchedWithoutForcePush(t *testing.T) {
	const prBranch = "goobers/impl/run-policy"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 61)

	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "61",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "true",
		"remediate":              "conflict",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "remediation policy") || !strings.Contains(stdout, "substantive") {
		t.Fatalf("stdout = %q, want a reason naming the excluded cause and policy", stdout)
	}

	st.mu.Lock()
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	if len(labels) != 1 || labels[0] != needsRemediationLabel {
		t.Fatalf("labels = %v, want %s left untouched (policy-excluded, no clear)", labels, needsRemediationLabel)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"true"`) ||
		!strings.Contains(string(data), `"policyExcluded":"true"`) ||
		!strings.Contains(string(data), "substantive") {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true, policyExcluded=true, reason naming substantive", data)
	}

	// The branch must NOT have been force-pushed: origin's PR branch tip is
	// unchanged from what initNonConflictingPRBranch seeded (verified by
	// checking out the branch fresh and confirming there's no rebase-onto-
	// main content, since a force-push here would have republished the
	// locally rebased/mechanically-clean commit).
	verify := t.TempDir()
	runGitT(t, verify, "clone", "--branch", prBranch, origin, filepath.Join(verify, "check"))
	if _, err := os.Stat(filepath.Join(verify, "check", "unrelated.txt")); err == nil {
		t.Fatal("origin's PR branch was force-pushed despite the policy exclusion — the finding would be silently dropped")
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
	if !strings.Contains(string(data), `"needsAgent":"true"`) ||
		!strings.Contains(string(data), `"conflict":"false"`) ||
		!strings.Contains(string(data), `"remediationCauses":"failing-ci"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true conflict=false cause=failing-ci", data)
	}

	verify := t.TempDir()
	runGitT(t, verify, "clone", "--branch", prBranch, origin, filepath.Join(verify, "check"))
	if _, err := os.Stat(filepath.Join(verify, "check", "unrelated.txt")); err != nil {
		t.Fatalf("origin's branch missing clean rebase before checkpoint routing: %v", err)
	}
}

func TestRebasePRSiblingOverlapHandoffDefersToCheckpoint(t *testing.T) {
	const prBranch = "goobers/impl/run-sibling-overlap"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	targetHeadSHA := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "HEAD"))
	verdict := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Location: "PR #66",
			Message:  "PR #67 must reconcile its overlap with PR #66.",
		}},
	})
	handoff, err := renderPostMergeRemediationHandoff(postMergeRemediationHandoff{
		DisplacingPullNumber: 66,
		TargetHeadSHA:        targetHeadSHA,
		Reason:               "file-overlap:shared.go",
		OverlappingFiles:     []string{"shared.go"},
	})
	if err != nil {
		t.Fatalf("renderPostMergeRemediationHandoff: %v", err)
	}
	st := &rebasePRServerState{
		labels:   []string{needsRemediationLabel},
		comments: []string{verdict, handoff},
	}
	server := st.start(t, "your-org", "your-repo", 67)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "67",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "true",
		"hasFailingCI":           "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"true"`) ||
		!strings.Contains(string(data), `"remediationCauses":"sibling-overlap"`) {
		t.Fatalf("rebase-result.json = %s, want matching stale substantive finding reclassified as sibling-overlap", data)
	}
	remoteHeadSHA := strings.TrimSpace(runGitOutputT(
		t, filepath.Dir(origin), "--git-dir="+origin, "rev-parse", "refs/heads/"+prBranch,
	))
	if remoteHeadSHA != targetHeadSHA {
		t.Fatalf("remote head = %q, want unchanged %q until sibling-overlap remediation is complete", remoteHeadSHA, targetHeadSHA)
	}
}

func TestRebasePRMultipleSiblingOverlapHandoffsShareOneCause(t *testing.T) {
	const prBranch = "goobers/impl/run-multiple-sibling-overlaps"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	targetHeadSHA := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "HEAD"))
	verdict := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Location: "PR #66",
			Message:  "PR #73 must reconcile its overlap with PR #66.",
		}},
	})
	legacy := `**Post-merge remediation handoff**

<!-- post-merge-remediation: {"displacingPullNumber":66,"reason":"file-overlap:first.go","overlappingFiles":["first.go"]} -->`
	newerLegacy := `**Post-merge remediation handoff**

<!-- post-merge-remediation: {"displacingPullNumber":72,"reason":"file-overlap:second.go","overlappingFiles":["second.go"]} -->`
	st := &rebasePRServerState{
		labels:   []string{needsRemediationLabel},
		comments: []string{verdict, legacy, newerLegacy},
	}
	server := st.start(t, "your-org", "your-repo", 73)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "73",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "true",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(wt.Path, "rebase-result.json"))
	if got := result["remediationCauses"]; got != "sibling-overlap" {
		t.Fatalf("remediationCauses = %q, want one sibling-overlap cause for both handoffs", got)
	}
	st.mu.Lock()
	migratedBodies := append([]string(nil), st.comments[1:]...)
	st.mu.Unlock()
	for i, body := range migratedBodies {
		migrated, ok := parsePostMergeRemediationHandoff(body)
		if !ok || migrated.Version != postMergeRemediationHandoffVersion ||
			migrated.TargetHeadSHA != targetHeadSHA {
			t.Fatalf("migrated handoff %d = %+v, ok=%v; want version %d pinned to %s",
				i, migrated, ok, postMergeRemediationHandoffVersion, targetHeadSHA)
		}
	}
}

func TestRebasePRClosedSiblingOverlapCannotBypassPolicy(t *testing.T) {
	const prBranch = "goobers/impl/run-closed-sibling-policy"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	targetHeadSHA := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "HEAD"))
	verdict := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Location: "PR #72",
			Message:  "PR #73 must reconcile its overlap with PR #72.",
		}},
	})
	handoff, err := renderPostMergeRemediationHandoff(postMergeRemediationHandoff{
		DisplacingPullNumber: 72,
		TargetHeadSHA:        targetHeadSHA,
		Reason:               "file-overlap:shared.go",
		OverlappingFiles:     []string{"shared.go"},
	})
	if err != nil {
		t.Fatalf("renderPostMergeRemediationHandoff: %v", err)
	}
	st := &rebasePRServerState{
		labels:   []string{needsRemediationLabel},
		comments: []string{verdict, handoff},
	}
	server := st.start(t, "your-org", "your-repo", 73)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "73",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "true",
		"remediate":              "substantive",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(wt.Path, "rebase-result.json"))
	if result["remediationCauses"] != "" ||
		result["policyExcluded"] != "true" ||
		!strings.Contains(fmt.Sprint(result["policyExcludedReason"]), "only detected cause(s) (sibling-overlap)") {
		t.Fatalf("policy output = causes=%q excluded=%q reason=%q, want only sibling-overlap excluded",
			result["remediationCauses"], result["policyExcluded"], result["policyExcludedReason"])
	}
}

func TestRebasePROpenSiblingOverlapPreservesIndependentSubstantiveCause(t *testing.T) {
	const prBranch = "goobers/impl/run-open-sibling-overlap"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 68)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "68",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "true",
		"hasSiblingOverlap":      "true",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(wt.Path, "rebase-result.json"))
	if got := result["remediationCauses"]; got != "substantive,sibling-overlap" {
		t.Fatalf("remediationCauses = %q, want independent substantive and sibling-overlap causes", got)
	}
}

func TestRebasePRMigratesTrustedLegacySiblingHandoff(t *testing.T) {
	const prBranch = "goobers/impl/run-legacy-sibling-overlap"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	targetHeadSHA := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "HEAD"))
	legacy := `**Post-merge remediation handoff**

<!-- post-merge-remediation: {"displacingPullNumber":66,"reason":"file-overlap:shared.go","overlappingFiles":["shared.go"]} -->`
	st := &rebasePRServerState{
		labels:   []string{needsRemediationLabel},
		comments: []string{legacy},
	}
	server := st.start(t, "your-org", "your-repo", 69)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber": "69",
		"head":           prBranch,
		"base":           "main",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(wt.Path, "rebase-result.json"))
	if got := result["remediationCauses"]; got != "sibling-overlap" {
		t.Fatalf("remediationCauses = %q, want migrated legacy sibling-overlap", got)
	}
	st.mu.Lock()
	migratedBody := st.comments[0]
	st.mu.Unlock()
	migrated, ok := parsePostMergeRemediationHandoff(migratedBody)
	if !ok || migrated.Version != postMergeRemediationHandoffVersion ||
		migrated.TargetHeadSHA != targetHeadSHA {
		t.Fatalf("migrated handoff = %+v, ok=%v; want version %d pinned to %s",
			migrated, ok, postMergeRemediationHandoffVersion, targetHeadSHA)
	}
}

func TestRebasePRLegacySiblingHandoffMigrationFailurePreservesCause(t *testing.T) {
	const prBranch = "goobers/impl/run-legacy-sibling-migration-failure"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	legacy := `**Post-merge remediation handoff**

<!-- post-merge-remediation: {"displacingPullNumber":66,"reason":"file-overlap:shared.go","overlappingFiles":["shared.go"]} -->`
	st := &rebasePRServerState{
		labels:              []string{needsRemediationLabel},
		comments:            []string{legacy},
		rejectCommentUpdate: true,
	}
	server := st.start(t, "your-org", "your-repo", 71)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber": "71",
		"head":           prBranch,
		"base":           "main",
	})

	code, _, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want migration failure", code, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(wt.Path, "rebase-result.json"))
	if got := result["remediationCauses"]; got != "sibling-overlap" {
		t.Fatalf("remediationCauses = %q, want detected sibling-overlap preserved on migration failure", got)
	}
}

func TestRebasePRResolvesDistinctAdjacentAdditions(t *testing.T) {
	const prBranch = "goobers/impl/run-adjacent"
	origin := initAdjacentAdditionPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 60)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "60",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "clean rebase") {
		t.Fatalf("stdout = %q, want automatic resolution to continue through the clean path", stdout)
	}

	const want = "items:\n  - existing\n  - from-base\n  - from-pr\n"
	got, err := os.ReadFile(filepath.Join(wt.Path, "items.yaml"))
	if err != nil {
		t.Fatalf("read resolved list: %v", err)
	}
	if string(got) != want {
		t.Fatalf("resolved list = %q, want %q", got, want)
	}

	verify := filepath.Join(t.TempDir(), "check")
	runGitT(t, filepath.Dir(verify), "clone", "--branch", prBranch, origin, verify)
	got, err = os.ReadFile(filepath.Join(verify, "items.yaml"))
	if err != nil {
		t.Fatalf("read pushed list: %v", err)
	}
	if string(got) != want {
		t.Fatalf("pushed list = %q, want %q", got, want)
	}

	st.mu.Lock()
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	if len(labels) != 0 {
		t.Fatalf("labels = %v, want remediation label cleared", labels)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"false"`) || !strings.Contains(string(data), `"conflict":"false"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=false conflict=false", data)
	}
}

func TestRebasePRRegeneratesPortalDistConflict(t *testing.T) {
	const prBranch = "goobers/impl/run-portal-dist"
	origin := initPortalDistConflictPRBranch(t, prBranch, false)
	wt := prWorktree(t, origin, prBranch)
	installPortalBuildMake(t)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 63)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "63",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "clean rebase") {
		t.Fatalf("stdout = %q, want regenerated bundle to continue through the clean path", stdout)
	}

	verify := filepath.Join(t.TempDir(), "check")
	runGitT(t, filepath.Dir(verify), "clone", "--branch", prBranch, origin, verify)
	got, err := os.ReadFile(filepath.Join(verify, portalDistPath, "index.html"))
	if err != nil {
		t.Fatalf("read regenerated portal bundle: %v", err)
	}
	if want := "from-base+from-pr\n"; string(got) != want {
		t.Fatalf("regenerated portal bundle = %q, want %q", got, want)
	}

	st.mu.Lock()
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	if len(labels) != 0 {
		t.Fatalf("labels = %v, want remediation label cleared", labels)
	}
}

func TestRebasePRDoesNotRegenerateMixedPortalConflict(t *testing.T) {
	const prBranch = "goobers/impl/run-mixed-portal-conflict"
	origin := initPortalDistConflictPRBranch(t, prBranch, true)
	wt := prWorktree(t, origin, prBranch)

	conflict, locations, _, err := attemptRebase(wt.Path, "main", "")
	if err != nil {
		t.Fatalf("attemptRebase() error = %v", err)
	}
	if !conflict {
		t.Fatal("attemptRebase() conflict = false, want mixed source and generated conflict preserved")
	}
	if len(locations) != 2 {
		t.Fatalf("attemptRebase() locations = %+v, want both mixed conflict paths", locations)
	}
	if unmerged := strings.TrimSpace(runGitOutputT(t, wt.Path, "diff", "--name-only", "--diff-filter=U")); unmerged != "" {
		t.Fatalf("unmerged paths = %q, want rebase aborted cleanly", unmerged)
	}
}

func TestRebasePRPreservesUnsafeEvidenceAfterAdjacentResolution(t *testing.T) {
	const prBranch = "goobers/impl/run-adjacent-then-structural"
	origin := initAdjacentThenStructuralConflictPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 62)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "62",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode rebase-result.json: %v", err)
	}
	if result["needsAgent"] != "true" || result["conflict"] != "true" {
		t.Fatalf("rebase-result.json = %s, want unsafe conflict routed to the agent", data)
	}
	if result["remediationCauses"] != "conflict" {
		t.Fatalf("remediationCauses = %q, want conflict", result["remediationCauses"])
	}
	var locations []rebaseConflictLocation
	if err := json.Unmarshal([]byte(result["conflictLocations"]), &locations); err != nil {
		t.Fatalf("decode conflict locations %q: %v", result["conflictLocations"], err)
	}
	if len(locations) != 1 || locations[0].Path != "status.go" || !strings.Contains(locations[0].Scope, "runStatus") {
		t.Fatalf("conflict locations = %+v, want status.go runStatus evidence", locations)
	}
	if unmerged := strings.TrimSpace(runGitOutputT(t, wt.Path, "diff", "--name-only", "--diff-filter=U")); unmerged != "" {
		t.Fatalf("unmerged paths = %q, want none after the aborted rebase", unmerged)
	}

	verify := filepath.Join(t.TempDir(), "check")
	runGitT(t, filepath.Dir(verify), "clone", "--branch", prBranch, origin, verify)
	list, err := os.ReadFile(filepath.Join(verify, "items.yaml"))
	if err != nil {
		t.Fatalf("read original PR list: %v", err)
	}
	if string(list) != "items:\n  - existing\n  - from-pr\n" {
		t.Fatalf("origin PR list = %q, want branch untouched after unsafe conflict", list)
	}
}

func TestRebasePRResolvesLiteralPathspecFilename(t *testing.T) {
	const prBranch = "goobers/impl/run-pathspec"
	const name = "[id].tsx"
	origin := initAdjacentConflictPRBranch(
		t,
		prBranch,
		name,
		"const items = [\n  \"existing\",\n]\n",
		"const items = [\n  \"existing\",\n  \"from-pr\",\n]\n",
		"const items = [\n  \"existing\",\n  \"from-base\",\n]\n",
		"",
	)
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 62)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "62",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "clean rebase") {
		t.Fatalf("stdout = %q, want literal path resolution to continue through the clean path", stdout)
	}

	const want = "const items = [\n  \"existing\",\n  \"from-base\",\n  \"from-pr\",\n]\n"
	got, err := os.ReadFile(filepath.Join(wt.Path, name))
	if err != nil {
		t.Fatalf("read resolved pathspec filename: %v", err)
	}
	if string(got) != want {
		t.Fatalf("resolved pathspec filename = %q, want %q", got, want)
	}
}

func TestRebasePRRejectsStructuralMarkerAdditions(t *testing.T) {
	const prBranch = "goobers/impl/run-structural"
	const ancestor = "func calculate() int {\n\treturn (\n\t\t- existing()\n\t)\n}\n"
	const incoming = "func calculate() int {\n\treturn (\n\t\t- existing()\n\t\t- fromPR()\n\t)\n}\n"
	const upstream = "func calculate() int {\n\treturn (\n\t\t- existing()\n\t\t- fromBase()\n\t)\n}\n"
	origin := initAdjacentConflictPRBranch(t, prBranch, "logic.txt", ancestor, incoming, upstream, "")
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 63)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "63",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "needs agentic remediation") {
		t.Fatalf("stdout = %q, want structural additions routed to agentic remediation", stdout)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"true"`) || !strings.Contains(string(data), `"conflict":"true"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true conflict=true", data)
	}
	if unmerged := strings.TrimSpace(runGitOutputT(t, wt.Path, "diff", "--name-only", "--diff-filter=U")); unmerged != "" {
		t.Fatalf("unmerged paths = %q, want none after the aborted structural conflict", unmerged)
	}

	verify := filepath.Join(t.TempDir(), "check")
	runGitT(t, filepath.Dir(verify), "clone", "--branch", prBranch, origin, verify)
	got, err := os.ReadFile(filepath.Join(verify, "logic.txt"))
	if err != nil {
		t.Fatalf("read original PR structural file: %v", err)
	}
	if string(got) != incoming {
		t.Fatalf("origin structural file = %q, want PR branch left untouched", got)
	}
}

func TestRebasePRRejectsPythonFunctionMarkerAdditions(t *testing.T) {
	const prBranch = "goobers/impl/run-python-structural"
	const ancestor = "def f():\n    - existing\n"
	const incoming = "def f():\n    - existing\n    - from_pr\n"
	const upstream = "def f():\n    - existing\n    - from_base\n"
	origin := initAdjacentConflictPRBranch(t, prBranch, "logic.py", ancestor, incoming, upstream, "")
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 64)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "64",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "needs agentic remediation") {
		t.Fatalf("stdout = %q, want Python structural additions routed to agentic remediation", stdout)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"true"`) || !strings.Contains(string(data), `"conflict":"true"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true conflict=true", data)
	}
	if unmerged := strings.TrimSpace(runGitOutputT(t, wt.Path, "diff", "--name-only", "--diff-filter=U")); unmerged != "" {
		t.Fatalf("unmerged paths = %q, want none after the aborted Python conflict", unmerged)
	}

	verify := filepath.Join(t.TempDir(), "check")
	runGitT(t, filepath.Dir(verify), "clone", "--branch", prBranch, origin, verify)
	got, err := os.ReadFile(filepath.Join(verify, "logic.py"))
	if err != nil {
		t.Fatalf("read original PR Python file: %v", err)
	}
	if string(got) != incoming {
		t.Fatalf("origin Python file = %q, want PR branch left untouched", got)
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
	attemptedHeadSHA := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "HEAD"))

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
	if !strings.Contains(string(data), `runStatus`) || !strings.Contains(string(data), `status.go`) {
		t.Fatalf("rebase-result.json = %s, want the conflicted function scope and path", data)
	}
	if !strings.Contains(string(data), `"rebaseBaseSha":"`) {
		t.Fatalf("rebase-result.json = %s, want the exact failed-rebase base SHA", data)
	}
	if !strings.Contains(string(data), `"attemptedHeadSha":"`+attemptedHeadSHA+`"`) {
		t.Fatalf("rebase-result.json = %s, want attempted head SHA %q", data, attemptedHeadSHA)
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

func TestRebasePRConflictInspectionFailurePreservesCause(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX git shim to inject a conflict-inspection failure")
	}
	const prBranch = "goobers/impl/run-conflict-inspection-failure"
	origin := initConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	rebaseBaseSHA := strings.TrimSpace(runGitOutputT(t, filepath.Dir(origin), "--git-dir="+origin, "rev-parse", "refs/heads/main"))

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	script := `#!/bin/sh
if [ "$1" = "diff" ] && [ "$2" = "--name-only" ] && [ "$3" = "--diff-filter=U" ] && [ "$4" = "-z" ]; then
	echo "conflict inspection rejected by test" >&2
	exit 1
fi
exec "$GOOBERS_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("GOOBERS_TEST_REAL_GIT", realGit)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 71)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "71",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, _, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want conflict-inspection failure", code, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(wt.Path, "rebase-result.json"))
	want := map[string]interface{}{
		"conflict":          "true",
		"remediationCauses": "conflict",
		"rebaseBaseSha":     rebaseBaseSHA,
	}
	for key, value := range want {
		if result[key] != value {
			t.Errorf("%s = %v, want %v", key, result[key], value)
		}
	}
	if !strings.Contains(fmt.Sprint(result[executor.OutputErrorMessage]), "inspect rebase conflict") {
		t.Errorf("error.message = %q, want conflict inspection context", result[executor.OutputErrorMessage])
	}
	if unmerged := strings.TrimSpace(runGitOutputT(t, wt.Path, "diff", "--name-only", "--diff-filter=U")); unmerged != "" {
		t.Fatalf("unmerged paths = %q, want the failed rebase aborted", unmerged)
	}
}

func TestRebasePRRejectsBinaryAttributedAdjacentAdditions(t *testing.T) {
	const prBranch = "goobers/impl/run-binary-adjacent"
	origin := initAttributedAdjacentAdditionPRBranch(t, prBranch, "*.yaml binary\n")
	wt := prWorktree(t, origin, prBranch)

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 61)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "61",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
	})

	code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"true"`) || !strings.Contains(string(data), `"conflict":"true"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=true conflict=true", data)
	}

	st.mu.Lock()
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	if len(labels) != 1 || labels[0] != needsRemediationLabel {
		t.Fatalf("labels = %v, want %s unchanged", labels, needsRemediationLabel)
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

func TestRebasePRPushFailurePreservesDownstreamContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX pre-receive hook to reject git push")
	}
	const prBranch = "goobers/impl/run-push-failure"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)
	attemptedHeadSHA := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "HEAD"))
	rebaseBaseSHA := strings.TrimSpace(runGitOutputT(t, filepath.Dir(origin), "--git-dir="+origin, "rev-parse", "refs/heads/main"))

	hook := filepath.Join(origin, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'push rejected by test' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}

	st := &rebasePRServerState{labels: []string{needsRemediationLabel}}
	server := st.start(t, "your-org", "your-repo", 65)
	instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
		"selectedNumber":         "65",
		"head":                   prBranch,
		"base":                   "main",
		"hasSubstantiveFindings": "false",
		"hasFailingCI":           "true",
	})

	code, _, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want rejected push failure", code, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(wt.Path, "rebase-result.json"))
	want := map[string]interface{}{
		"selectedNumber":         "65",
		"head":                   prBranch,
		"needsAgent":             "true",
		"conflict":               "false",
		"conflictLocations":      "[]",
		"attemptedHeadSha":       attemptedHeadSHA,
		"rebaseBaseSha":          rebaseBaseSHA,
		"remediationCauses":      "failing-ci",
		"policyExcluded":         "false",
		"policyExcludedReason":   "",
		executor.OutputErrorCode: errorCodeProvider,
	}
	for key, value := range want {
		if result[key] != value {
			t.Errorf("%s = %v, want %v", key, result[key], value)
		}
	}
}

// TestRebasePRFailureWritesDownstreamContract proves rebase-pr fails closed
// before any git/provider call when a required capability is absent while
// preserving every output needed to route through remediation-checkpoint.
func TestRebasePRFailureWritesDownstreamContract(t *testing.T) {
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
	result := readProviderStageResult(t, filepath.Join(workDir, "rebase-result.json"))
	want := map[string]interface{}{
		"selectedNumber":         "58",
		"head":                   "goobers/impl/run-d",
		"needsAgent":             "true",
		"conflict":               "false",
		"conflictLocations":      "[]",
		"attemptedHeadSha":       "",
		"rebaseBaseSha":          "",
		"remediationCauses":      "",
		executor.OutputErrorCode: errorCodeProvider,
	}
	for key, value := range want {
		if result[key] != value {
			t.Errorf("%s = %v, want %v", key, result[key], value)
		}
	}
}

func TestRebasePRFailurePreservesSiblingPolicyExclusion(t *testing.T) {
	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-363-sibling-nocap")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "70")
	t.Setenv("GOOBERS_INPUT_HEAD", "goobers/impl/run-sibling-nocap")
	t.Setenv("GOOBERS_INPUT_HASSIBLINGOVERLAP", "true")
	t.Setenv("GOOBERS_INPUT_HASSUBSTANTIVEFINDINGS", "true")
	t.Setenv("GOOBERS_INPUT_REMEDIATE", "conflict")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "rebase-pr", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1", code, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(workDir, "rebase-result.json"))
	if result["remediationCauses"] != "" ||
		result["policyExcluded"] != "true" ||
		!strings.Contains(fmt.Sprint(result["policyExcludedReason"]), "sibling-overlap") {
		t.Fatalf("failure policy output = causes=%q excluded=%q reason=%q, want sibling-overlap exclusion preserved",
			result["remediationCauses"], result["policyExcluded"], result["policyExcludedReason"])
	}
}
