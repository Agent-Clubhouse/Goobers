package premergecheck

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

// fixture is an origin repository plus a working clone of it, each holding a
// one-package Go module.
type fixture struct {
	t      *testing.T
	origin string
	clone  string
	author string
}

func testGit(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := testgit.CommandContext(ctx, args...)
	cmd.Dir = dir
	return cmd
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := testGit(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) commit(branch string, files map[string]string, remove ...string) string {
	f.t.Helper()
	if f.git(f.author, "branch", "--show-current") != branch {
		f.git(f.author, "checkout", "-q", branch)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(f.author, name), []byte(body), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	for _, name := range remove {
		f.git(f.author, "rm", "-q", name)
	}
	f.git(f.author, "add", "-A")
	f.git(f.author, "commit", "-q", "-m", "change on "+branch)
	f.git(f.author, "push", "-q", "origin", branch)
	return f.git(f.author, "rev-parse", "HEAD")
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, origin: filepath.Join(root, "origin.git"), clone: filepath.Join(root, "clone"), author: filepath.Join(root, "author")}
	f.git(root, "init", "-q", "--bare", "-b", "main", f.origin)
	f.git(root, "init", "-q", "-b", "main", f.author)
	f.git(f.author, "remote", "add", "origin", f.origin)
	f.commit("main", map[string]string{
		"go.mod": "module example.com/fixture\n\ngo 1.21\n",
		"lib.go": "package fixture\n\nfunc Helper() int { return 1 }\n",
	})
	f.git(root, "clone", "-q", f.origin, f.clone)
	return f
}

func (f *fixture) branchFromMain(name string) {
	f.t.Helper()
	f.git(f.author, "checkout", "-q", "-b", name, "main")
}

func (f *fixture) request(headSHA string) Request {
	return Request{
		RepoDir:    f.clone,
		Remote:     f.origin,
		BaseBranch: "main",
		HeadRef:    "feature",
		HeadSHA:    headSHA,
		Commands:   ParseCommands("go build ./...\ngo vet ./..."),
		Env:        append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod"),
		Git:        testGit,
	}
}

func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
}

// TestRunRefusesStaleBaseCompileCollision reproduces #7132's collision: the PR
// builds against the base it branched from, but main has since removed the
// symbol it calls, so only the merge result fails to compile.
func TestRunRefusesStaleBaseCompileCollision(t *testing.T) {
	requireGo(t)
	f := newFixture(t)
	f.branchFromMain("feature")
	head := f.commit("feature", map[string]string{"use.go": "package fixture\n\nfunc Use() int { return Helper() }\n"})
	tip := f.commit("main", map[string]string{"lib.go": "package fixture\n\nfunc Renamed() int { return 1 }\n"})

	// The PR head alone is green: the collision exists only on the merge.
	alone := f.request(head)
	alone.BaseBranch = "feature"
	if res, err := Run(context.Background(), alone); err != nil || res.Outcome != OutcomePassed {
		t.Fatalf("head alone = %+v, %v; want passed", res, err)
	}

	res, err := Run(context.Background(), f.request(head))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeFailed || res.Command != "go build ./..." {
		t.Fatalf("result = %+v, want go build failure", res)
	}
	if !strings.Contains(res.Output, "Helper") {
		t.Fatalf("output %q does not name the missing symbol", res.Output)
	}
	if res.BaseTipSHA != tip {
		t.Fatalf("BaseTipSHA = %s, want current main tip %s", res.BaseTipSHA, tip)
	}
	if list := f.git(f.clone, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("disposable worktree leaked:\n%s", list)
	}
}

// TestRunPassesDisjointBaseMovement also fetches the head through the pull
// request ref, the form that serves fork heads from the base repository.
func TestRunPassesDisjointBaseMovement(t *testing.T) {
	requireGo(t)
	f := newFixture(t)
	f.branchFromMain("feature")
	head := f.commit("feature", map[string]string{"use.go": "package fixture\n\nfunc Use() int { return Helper() }\n"})
	f.git(f.author, "push", "-q", "origin", "feature:refs/pull/7/head")
	f.commit("main", map[string]string{"other.go": "package fixture\n\nfunc Other() int { return 2 }\n"})

	req := f.request(head)
	req.HeadRef = "refs/pull/7/head"
	res, err := Run(context.Background(), req)
	if err != nil || res.Outcome != OutcomePassed {
		t.Fatalf("Run = %+v, %v; want passed", res, err)
	}
}

func TestRunReportsConflictAgainstBaseTip(t *testing.T) {
	f := newFixture(t)
	f.branchFromMain("feature")
	head := f.commit("feature", map[string]string{"lib.go": "package fixture\n\nfunc Helper() int { return 2 }\n"})
	f.commit("main", map[string]string{"lib.go": "package fixture\n\nfunc Helper() int { return 3 }\n"})

	req := f.request(head)
	req.Commands = [][]string{{"unreachable-command"}}
	res, err := Run(context.Background(), req)
	if err != nil || res.Outcome != OutcomeConflict {
		t.Fatalf("Run = %+v, %v; want merge-conflict", res, err)
	}
}

func TestRunReportsHeadMovedFromPin(t *testing.T) {
	f := newFixture(t)
	f.branchFromMain("feature")
	pinned := f.commit("feature", map[string]string{"use.go": "package fixture\n"})
	moved := f.commit("feature", map[string]string{"more.go": "package fixture\n"})

	req := f.request(pinned)
	req.Commands = [][]string{{"unreachable-command"}}
	res, err := Run(context.Background(), req)
	if err != nil || res.Outcome != OutcomeHeadMoved || res.HeadTipSHA != moved {
		t.Fatalf("Run = %+v, %v; want head-moved to %s, nothing measured", res, err, moved)
	}
}

func TestRunFailsClosedWhenCommandCannotStart(t *testing.T) {
	f := newFixture(t)
	f.branchFromMain("feature")
	head := f.commit("feature", map[string]string{"use.go": "package fixture\n"})

	req := f.request(head)
	req.Commands = [][]string{{"goobers-no-such-check-binary"}}
	if res, err := Run(context.Background(), req); err == nil {
		t.Fatalf("Run = %+v, want an error: an unrunnable check must not read as a measurement", res)
	}
}

func TestParseCommandsSkipsBlankAndCommentLines(t *testing.T) {
	got := ParseCommands("\n# compile\ngo build ./...\r\n  go vet -tags integration ./...  \n")
	if len(got) != 2 || strings.Join(got[0], " ") != "go build ./..." || strings.Join(got[1], " ") != "go vet -tags integration ./..." {
		t.Fatalf("ParseCommands = %q", got)
	}
}
