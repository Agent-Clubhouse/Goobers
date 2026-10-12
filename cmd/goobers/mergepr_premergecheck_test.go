package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

// preMergeFixture is an origin repository holding a one-package Go module on
// main and a PR branch cut from it, plus an author clone that advances both.
type preMergeFixture struct {
	t      *testing.T
	origin string
	author string
}

func (f *preMergeFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := testgit.CommandContext(context.Background(), append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *preMergeFixture) commit(branch, name, body string) string {
	f.t.Helper()
	if f.git(f.author, "branch", "--show-current") != branch {
		f.git(f.author, "checkout", "-q", branch)
	}
	if err := os.WriteFile(filepath.Join(f.author, name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(f.author, "add", "-A")
	f.git(f.author, "commit", "-q", "-m", "change "+name)
	f.git(f.author, "push", "-q", "origin", branch)
	return f.git(f.author, "rev-parse", "HEAD")
}

// newPreMergeFixture returns the fixture plus the PR head and the base SHA the
// PR's CI ran against. mainChange is committed to main after the PR branched.
func newPreMergeFixture(t *testing.T, head, mainChange string) (f *preMergeFixture, headSHA, staleBase string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	root := t.TempDir()
	f = &preMergeFixture{t: t, origin: filepath.Join(root, "origin.git"), author: filepath.Join(root, "author")}
	f.git(root, "init", "-q", "--bare", "-b", "main", f.origin)
	f.git(root, "init", "-q", "-b", "main", f.author)
	f.git(f.author, "remote", "add", "origin", f.origin)
	f.commit("main", "go.mod", "module example.com/fixture\n\ngo 1.21\n")
	staleBase = f.commit("main", "lib.go", "package fixture\n\nfunc Helper() int { return 1 }\n")
	f.git(f.author, "checkout", "-q", "-b", "feature")
	headSHA = f.commit("feature", "use.go", head)
	f.git(f.author, "push", "-q", "origin", "feature:refs/pull/9/head")
	f.commit("main", "lib.go", mainChange)
	return f, headSHA, staleBase
}

func runPreMergeCheckedMergePR(t *testing.T, mainChange string) (*mergePRServerState, int, string, map[string]interface{}) {
	t.Helper()
	f, headSHA, staleBase := newPreMergeFixture(t, "package fixture\n\nfunc Use() int { return Helper() }\n", mainChange)
	st := &mergePRServerState{checkState: "success", headSHA: headSHA, baseSHA: staleBase, headBranch: "fork-feature"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": headSHA, "baseSha": staleBase,
		"preMergeCheck": "go build ./...\ngo vet ./...",
	})
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	f.git(dir, "clone", "-q", f.origin, ".")

	code, _, stderr := runArgs(t, "merge-pr", root)
	return st, code, stderr, readMergeResult(t, dir)
}

// TestMergePRRefusesStaleBaseCompileCollision is #7132's acceptance: the PR is
// green against the base its CI used, but main has since removed a symbol the
// PR calls. The managed merge path must refuse instead of breaking main.
func TestMergePRRefusesStaleBaseCompileCollision(t *testing.T) {
	st, code, stderr, result := runPreMergeCheckedMergePR(t, "package fixture\n\nfunc Renamed() int { return 1 }\n")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if st.mergeCalls != 0 || st.enqueueCalls != 0 {
		t.Fatalf("merge calls = %d, enqueue calls = %d; a stale-base compile failure must never land", st.mergeCalls, st.enqueueCalls)
	}
	reason, _ := result["reason"].(string)
	if merged, _ := result["merged"].(bool); merged || !strings.HasPrefix(reason, preMergeCheckFailedReason+": `go build ./...`") || !strings.Contains(reason, "Helper") {
		t.Fatalf("result = %+v, want a pre-merge-check-failed refusal naming the missing symbol", result)
	}
}

func TestMergePRLandsWhenMergeResultStillBuilds(t *testing.T) {
	st, code, stderr, result := runPreMergeCheckedMergePR(t, "package fixture\n\nfunc Helper() int { return 1 }\n\nfunc Other() int { return 2 }\n")
	if code != 0 || st.mergeCalls != 1 {
		t.Fatalf("code = %d, merge calls = %d, stderr = %q, result = %+v; want a merge", code, st.mergeCalls, stderr, result)
	}
}

// TestMergePRFailsClosedWhenPreMergeCheckCannotRun pins that an unmeasured
// merge is never landed: a workspace that is not a checkout fails the stage.
func TestMergePRFailsClosedWhenPreMergeCheckCannotRun(t *testing.T) {
	st := &mergePRServerState{checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
		"preMergeCheck": "go build ./...",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 1 || st.mergeCalls != 0 {
		t.Fatalf("code = %d, merge calls = %d, stderr = %q; want a fail-closed stage error", code, st.mergeCalls, stderr)
	}
	if result := readMergeResult(t, dir); result["merged"] != false {
		t.Fatalf("result = %+v, want merged=false", result)
	}
}
