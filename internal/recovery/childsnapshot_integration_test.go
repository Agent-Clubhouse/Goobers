//go:build integration

package recovery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func childSnapshotFixture(t *testing.T) (string, string, time.Time) {
	t.Helper()
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	childSnapshotWrite(t, repository, "tracked.txt", "base\n")
	childSnapshotWrite(t, repository, "deleted.txt", "delete me\n")
	childSnapshotWrite(t, repository, ".gitignore", "ignored.out\n")
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "base")
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}).CanonicalKey()
	return repository, key, time.Date(2026, 10, 4, 0, 0, 0, 123, time.UTC)
}
func childSnapshotWrite(t *testing.T, repository, name, content string) {
	t.Helper()
	p := filepath.Join(repository, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func captureChildFixture(t *testing.T, repository, key, id string, at time.Time, policy SnapshotPolicy) ChildSnapshot {
	t.Helper()
	snapshot, err := CaptureChildSnapshot(context.Background(), repository, key, id, at, at.Add(24*time.Hour), policy)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestIntegrationChildSnapshotFiltersWithoutChangingParent(t *testing.T) {
	repository, key, at := childSnapshotFixture(t)
	ctx := context.Background()
	policy := SnapshotPolicy{ExcludedPaths: []string{"credentials/token"}}
	for name, content := range map[string]string{"tracked.txt": "worktree\r\n", "new.bin": "\x00\xff\x01", "ignored.out": "skip", ".goobers/token": "runtime secret", ".goober-assets/instructions": "runtime", ".goobers-launcher-session-1/config": "launcher", "mutations.jsonl": "receipt", "credentials/token": "provider secret"} {
		childSnapshotWrite(t, repository, name, content)
	}
	recoveryTestGit(t, repository, "add", "--force", ".goobers/token", "credentials/token")
	if err := os.Remove(filepath.Join(repository, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	beforeHead := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	indexPath := filepath.Join(repository, ".git", "index")
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := captureChildFixture(t, repository, key, "parent", at, policy)
	again := captureChildFixture(t, repository, key, "parent", at, policy)
	if snapshot.Record.SnapshotSHA != again.Record.SnapshotSHA || snapshot.IndexDigest != again.IndexDigest {
		t.Fatal("snapshot identity changed on retry")
	}
	for _, name := range []string{"deleted.txt", "ignored.out", ".goobers", ".goober-assets", ".goobers-launcher-session-1", "mutations.jsonl", "credentials"} {
		if got := recoveryTestGit(t, repository, "ls-tree", "--name-only", snapshot.Record.SnapshotSHA, "--", name); got != "" {
			t.Errorf("excluded or deleted path captured: %s", got)
		}
	}
	if got := recoveryTestGit(t, repository, "show", snapshot.Record.SnapshotSHA+":new.bin"); got != "\x00\xff\x01" {
		t.Fatalf("binary snapshot=%q", got)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatal("capture moved HEAD")
	}
	afterIndex, _ := os.ReadFile(indexPath)
	if !bytes.Equal(beforeIndex, afterIndex) {
		t.Fatal("capture changed index")
	}
	if got, _ := os.ReadFile(filepath.Join(repository, "tracked.txt")); string(got) != "worktree\r\n" {
		t.Fatal("capture changed parent files")
	}
	var archive bytes.Buffer
	published, err := WriteChildSnapshotBundle(ctx, repository, snapshot, &archive, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if published.Record.ArchiveFormat != "delta" || published.Record.ArchiveBytes != int64(archive.Len()) {
		t.Fatalf("carrier=%+v", published.Record)
	}
	var tooSmall bytes.Buffer
	if _, err := WriteChildSnapshotBundle(ctx, repository, snapshot, &tooSmall, 8); err == nil || tooSmall.Len() > 8 {
		t.Fatal("carrier byte bound not enforced")
	}
	archivePath := filepath.Join(t.TempDir(), "child.bundle")
	if err := os.WriteFile(archivePath, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	recoveryTestGit(t, destination, "init")
	if err := ImportChildSnapshot(ctx, destination, archivePath, published, 1<<20); err == nil {
		t.Fatal("delta imported without prerequisite")
	}
	recoveryTestGit(t, destination, "fetch", repository, beforeHead)
	if err := ImportChildSnapshot(ctx, destination, archivePath, published, 1<<20); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", "tracked.txt")
	current, err := CaptureChildSnapshot(ctx, repository, snapshot.Record.RepositoryKey, snapshot.Record.RunID, snapshot.Record.CreatedAt, snapshot.Record.RetainUntil, snapshot.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if current.IndexDigest == snapshot.IndexDigest {
		t.Fatal("snapshot receipt did not distinguish a staging-only change")
	}
}

func TestIntegrationChildSnapshotSymlinkAndNestedRefusal(t *testing.T) {
	repository, key, at := childSnapshotFixture(t)
	if err := os.Symlink("tracked.txt", filepath.Join(repository, "safe-link")); err != nil {
		t.Fatal(err)
	}
	captureChildFixture(t, repository, key, "safe", at, SnapshotPolicy{})
	if err := os.Symlink(".goobers/token", filepath.Join(repository, "credential-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureChildSnapshot(context.Background(), repository, key, "unsafe", at, at.Add(time.Hour), SnapshotPolicy{}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("credential symlink accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(repository, "credential-link")); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repository, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, nested, "init")
	recoveryTestGit(t, nested, "commit", "--allow-empty", "-m", "nested")
	if _, err := CaptureChildSnapshot(context.Background(), repository, key, "nested", at, at.Add(time.Hour), SnapshotPolicy{}); !errors.Is(err, ErrNestedRecoveryRequired) {
		t.Fatalf("nested repo accepted: %v", err)
	}
}
