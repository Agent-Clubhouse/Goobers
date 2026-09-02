package workspacedelta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

// testGit is the Git a test supplies: the isolated testgit command with no
// per-caller environment, which is exactly what the two production callers
// vary and the shared mechanics must not depend on.
type testGit struct{}

func (testGit) Run(_ context.Context, dir string, args ...string) error {
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Join(err, errors.New(strings.TrimSpace(string(out))))
	}
	return nil
}

func (testGit) Output(_ context.Context, dir string, args ...string) (string, error) {
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initOrigin creates a bare origin with one commit on main.
func initOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	run(t, root, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	run(t, root, "clone", origin, seed)
	run(t, seed, "config", "user.name", "seed")
	run(t, seed, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "add", "README.md")
	run(t, seed, "commit", "-m", "seed")
	run(t, seed, "push", "origin", "main")
	return origin
}

func clone(t *testing.T, origin, branch string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "checkout")
	run(t, filepath.Dir(dir), "clone", "--quiet", "--branch", branch, origin, dir)
	run(t, dir, "config", "user.name", "test")
	run(t, dir, "config", "user.email", "test@example.com")
	return dir
}

func commit(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", file)
	run(t, dir, "commit", "-q", "-m", "commit "+file)
	return run(t, dir, "rev-parse", "HEAD")
}

// The carrier round trip: a bundle created in one checkout lands in another
// that only ever cloned base, and in a BARE MIRROR — the two receiving
// shapes the pod and the worker are.
func TestCreateFetchRoundTripIntoCheckoutAndMirror(t *testing.T) {
	ctx := context.Background()
	origin := initOrigin(t)
	src := clone(t, origin, "main")
	run(t, src, "checkout", "-q", "-b", "goobers/wf/run-1")
	want := commit(t, src, "carried.txt", "work\n")

	b, err := Create(ctx, testGit{}, src, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.Tip != want || b.Base == "" || b.Base == want || b.Digest != Digest(b.Data) {
		t.Fatalf("bundle = {digest %s base %s tip %s}, want tip %s and a distinct base", b.Digest, b.Base, b.Tip, want)
	}
	if out := run(t, src, "for-each-ref", "--format=%(refname)", Ref); out != "" {
		t.Fatalf("delta ref left behind in the source repo: %q", out)
	}

	checkout := clone(t, origin, "main")
	tip, err := Fetch(ctx, testGit{}, checkout, b)
	if err != nil {
		t.Fatalf("Fetch into checkout: %v", err)
	}
	if tip != want {
		t.Fatalf("fetched tip = %s, want %s", tip, want)
	}

	mirror := filepath.Join(t.TempDir(), "repo.git")
	run(t, t.TempDir(), "clone", "--quiet", "--mirror", origin, mirror)
	tip, err = Fetch(ctx, testGit{}, mirror, b)
	if err != nil {
		t.Fatalf("Fetch into mirror: %v", err)
	}
	if tip != want {
		t.Fatalf("mirror fetched tip = %s, want %s", tip, want)
	}
}

// Load/Fetch refuse bytes that do not hash to their digest: the blob plane
// does not verify content addressing, so the receiver must.
func TestLoadRefusesASubstitutedBundle(t *testing.T) {
	lie := Digest([]byte("some other content entirely"))
	if _, err := Load([]byte("not a bundle"), lie); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Load = %v, want a digest mismatch refusal", err)
	}
	if _, err := Fetch(context.Background(), testGit{}, t.TempDir(), Bundle{Digest: lie, Data: []byte("not a bundle")}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Fetch = %v, want a digest mismatch refusal before git is touched", err)
	}
}

// The ancestry guard's arms (#3821), classified once for every receiver.
func TestReconcileArms(t *testing.T) {
	ctx := context.Background()
	origin := initOrigin(t)
	base := run(t, clone(t, origin, "main"), "rev-parse", "HEAD")

	// A publisher one commit past base.
	pub := clone(t, origin, "main")
	run(t, pub, "checkout", "-q", "-b", "goobers/wf/run-guard")
	first := commit(t, pub, "first.txt", "first\n")
	// And a second, further commit on the same line.
	second := commit(t, pub, "second.txt", "second\n")

	// A receiver holding the whole line, for the ancestry queries.
	repo := clone(t, origin, "main")
	run(t, repo, "fetch", "-q", pub, "goobers/wf/run-guard")

	t.Run("absent ref -> create", func(t *testing.T) {
		got, err := Reconcile(ctx, testGit{}, repo, "sha256:d", "", first, "origin/main")
		if err != nil || got != OutcomeCreate {
			t.Fatalf("Reconcile = %v, %v; want create", got, err)
		}
	})
	t.Run("ref at base, tip ahead -> fast-forward", func(t *testing.T) {
		got, err := Reconcile(ctx, testGit{}, repo, "sha256:d", base, first, "origin/main")
		if err != nil || got != OutcomeFastForward {
			t.Fatalf("Reconcile = %v, %v; want fast-forward", got, err)
		}
	})
	t.Run("ref equal to tip -> fast-forward (no-op move)", func(t *testing.T) {
		got, err := Reconcile(ctx, testGit{}, repo, "sha256:d", first, first, "origin/main")
		if err != nil || got != OutcomeFastForward {
			t.Fatalf("Reconcile = %v, %v; want fast-forward", got, err)
		}
	})
	t.Run("ref ahead of a stale tip -> keep", func(t *testing.T) {
		got, err := Reconcile(ctx, testGit{}, repo, "sha256:d", second, first, "origin/main")
		if err != nil || got != OutcomeKeep {
			t.Fatalf("Reconcile = %v, %v; want keep", got, err)
		}
	})
	t.Run("ref is an advanced base -> base drift, applies", func(t *testing.T) {
		// Base moves on origin after the delta was cut.
		other := clone(t, origin, "main")
		newBase := commit(t, other, "newmain.txt", "new\n")
		run(t, other, "push", "-q", "origin", "main")
		run(t, repo, "fetch", "-q", "origin")
		got, err := Reconcile(ctx, testGit{}, repo, "sha256:d", newBase, first, "origin/main")
		if err != nil || got != OutcomeBaseDrift {
			t.Fatalf("Reconcile = %v, %v; want base-drift", got, err)
		}
		// With no base ref to consult, the same shape is real divergence.
		var diverged *DivergedError
		if _, err := Reconcile(ctx, testGit{}, repo, "sha256:d", newBase, first, ""); !errors.As(err, &diverged) {
			t.Fatalf("Reconcile without a base ref = %v, want DivergedError", err)
		}
	})
	t.Run("tip is a rebase of the ref -> rebase, applies", func(t *testing.T) {
		// #4175, the shape rebase-pr produces: the PR branch is replayed onto
		// an advanced base, so the new tip is NOT a descendant of the old head
		// and never can be. Nothing on the ref is lost by landing it — every
		// commit it carries is in the tip by patch id.
		side := clone(t, origin, "main")
		run(t, side, "fetch", "-q", pub, "goobers/wf/run-guard")
		run(t, side, "checkout", "-q", "-b", "topic", first)
		moved := commit(t, side, "movedmain.txt", "moved\n")
		run(t, side, "checkout", "-q", "main")
		run(t, side, "reset", "-q", "--hard", moved)
		run(t, side, "checkout", "-q", "topic")
		run(t, side, "reset", "-q", "--hard", second)
		run(t, side, "rebase", "-q", "main")
		rebased := run(t, side, "rev-parse", "HEAD")
		run(t, repo, "fetch", "-q", side, "topic")

		got, err := Reconcile(ctx, testGit{}, repo, "sha256:d", second, rebased, "origin/main")
		if err != nil || got != OutcomeRebase {
			t.Fatalf("Reconcile = %v, %v; want rebase", got, err)
		}
		if got.String() != "rebase" {
			t.Fatalf("String() = %q, want %q", got.String(), "rebase")
		}
		// No base ref to consult does not change the answer: this arm asks
		// only about the two commits it was given.
		if got, err := Reconcile(ctx, testGit{}, repo, "sha256:d", second, rebased, ""); err != nil || got != OutcomeRebase {
			t.Fatalf("Reconcile without a base ref = %v, %v; want rebase", got, err)
		}

		// And a ref that moved on after the rebase was cut is still refused:
		// its extra commit has no equivalent in the tip, so landing the delta
		// would lose it.
		run(t, side, "checkout", "-q", second)
		run(t, side, "checkout", "-q", "-b", "advanced")
		advanced := commit(t, side, "advanced.txt", "advanced\n")
		run(t, repo, "fetch", "-q", side, "advanced")
		var diverged *DivergedError
		if _, err := Reconcile(ctx, testGit{}, repo, "sha256:d", advanced, rebased, "origin/main"); !errors.As(err, &diverged) {
			t.Fatalf("Reconcile for an advanced ref = %v, want DivergedError", err)
		}
	})
	t.Run("genuine divergence -> named error with both SHAs", func(t *testing.T) {
		fork := clone(t, origin, "main")
		run(t, fork, "fetch", "-q", pub, "goobers/wf/run-guard")
		run(t, fork, "checkout", "-q", first)
		forked := commit(t, fork, "fork.txt", "fork\n")
		run(t, repo, "fetch", "-q", fork, forked)
		_, err := Reconcile(ctx, testGit{}, repo, "sha256:d", forked, second, "origin/main")
		var diverged *DivergedError
		if !errors.As(err, &diverged) {
			t.Fatalf("Reconcile = %v, want DivergedError", err)
		}
		if diverged.Current != forked || diverged.Tip != second || !strings.Contains(err.Error(), forked) || !strings.Contains(err.Error(), second) {
			t.Fatalf("DivergedError = %+v (%v), want both SHAs named", diverged, err)
		}
	})
}
