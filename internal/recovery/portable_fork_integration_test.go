//go:build integration

package recovery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIntegrationPortableForkPreservesRealAncestryAndExcludedBaseline(t *testing.T) {
	repo, key, at := childSnapshotFixture(t)
	childSnapshotWrite(t, repo, "private/template.txt", "baseline value")
	recoveryTestGit(t, repo, "add", "private/template.txt")
	recoveryTestGit(t, repo, "commit", "-m", "baseline")
	base := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	childSnapshotWrite(t, repo, "private/template.txt", "injected secret")
	childSnapshotWrite(t, repo, "private/token", "new secret")
	childSnapshotWrite(t, repo, "tracked.txt", "returned edits")
	policy := SnapshotPolicy{ExcludedPaths: []string{"private"}}
	carrier := func() PortableCarrier {
		t.Helper()
		snapshot := captureChildFixture(t, repo, key, "owner", at, policy)
		var bundle bytes.Buffer
		portable, err := WritePortableSnapshot(t.Context(), repo, snapshot, &bundle, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return PortableCarrier{Snapshot: portable, Bundle: bundle.Bytes()}
	}
	input := carrier()
	first, err := PreparePortableFork(t.Context(), repo, base, "owner-p1-b1", input, at)
	if err != nil {
		t.Fatal(err)
	}
	again, err := PreparePortableFork(t.Context(), repo, base, "owner-p1-b1", input, at)
	if err != nil || again.SnapshotSHA != first.SnapshotSHA {
		t.Fatal("fork replay changed", again, err)
	}
	recoveryTestGit(t, repo, "merge-base", "--is-ancestor", base, first.SnapshotSHA)
	if got := recoveryTestGit(t, repo, "rev-parse", first.SnapshotSHA+"^"); got != base {
		t.Fatal("synthetic parent escaped transport", got)
	}
	if got := recoveryTestGit(t, repo, "show", first.SnapshotSHA+":private/template.txt"); got != "baseline value" {
		t.Fatal("excluded baseline was deleted or replaced", got)
	}
	if got := recoveryTestGit(t, repo, "ls-tree", "--name-only", first.SnapshotSHA, "--", "private/token"); got != "" {
		t.Fatal("injected token entered host fork", got)
	}
	if got := recoveryTestGit(t, repo, "show", first.SnapshotSHA+":tracked.txt"); got != "returned edits" {
		t.Fatal("verified contribution lost", got)
	}
	if got := recoveryTestGit(t, repo, "rev-parse", "HEAD"); got != base {
		t.Fatal("fork moved source HEAD")
	}
	after, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil || !bytes.Equal(index, after) {
		t.Fatal("fork moved source index", err)
	}
	childSnapshotWrite(t, repo, "tracked.txt", "changed replay")
	if _, err := PreparePortableFork(t.Context(), repo, base, "owner-p1-b1", carrier(), at); !errors.Is(err, ErrRecordConflict) {
		t.Fatal("fork CAS accepted changed input", err)
	}
}
