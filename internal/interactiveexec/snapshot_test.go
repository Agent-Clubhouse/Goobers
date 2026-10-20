package interactiveexec

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/harness"
	auth "github.com/goobers/goobers/internal/interactiveauth"
	"github.com/goobers/goobers/internal/invoke"
)

func snapshotExecutor(t *testing.T) (*Executor, string) {
	t.Helper()
	principal, err := auth.LocalPrincipal("snapshot-owner")
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{StateDir: t.TempDir(), SourceDir: t.TempDir(), Mode: "local-tools", Principal: principal})
	if err != nil {
		t.Fatal(err)
	}
	e.runProcess = func(context.Context, string, []string, func(harness.TranscriptDelta) error) (harness.ProcessResult, error) {
		panic("filesystem snapshot must never launch a command")
	}
	parent, _ := e.workspace("snapshot-session")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.metadata(parent, "baseline"), []byte(strings.Repeat("a", 40)), 0600); err != nil {
		t.Fatal(err)
	}
	return e, parent
}

func TestCaptureRunSnapshotIsIndependentPrivateAndExcludesSecrets(t *testing.T) {
	e, parent := snapshotExecutor(t)
	for name, content := range map[string]string{"invoice.mjs": "original", ".gitignore": "build/", ".env": "secret", "credentials": "secret", "client.pem": "secret", "auth.json": "secret"} {
		if err := os.WriteFile(filepath.Join(parent, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(parent, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := e.CaptureRunSnapshot(context.Background(), "snapshot-owner", "snapshot-session", "run-one")
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(e.config.StateDir, "native-snapshots", "snapshot-session", fmt.Sprintf("%x", sha256.Sum256([]byte("run-one"))), "source")
	if snapshot != expected {
		t.Fatalf("unexpected snapshot location %s", snapshot)
	}
	for _, name := range []string{".git", ".env", "credentials", "client.pem", "auth.json"} {
		if _, err := os.Lstat(filepath.Join(snapshot, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("copied credential/private entry %s", name)
		}
	}
	parentInfo, _ := os.Stat(filepath.Join(parent, "invoice.mjs"))
	snapshotInfo, err := os.Stat(filepath.Join(snapshot, "invoice.mjs"))
	if err != nil || os.SameFile(parentInfo, snapshotInfo) {
		t.Fatal("snapshot shares parent file identity")
	}
	if err := os.WriteFile(filepath.Join(snapshot, "invoice.mjs"), []byte("independent edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(parent, "invoice.mjs")); err != nil || string(content) != "original" {
		t.Fatal("snapshot edit changed parent")
	}
	for path := snapshot; path != e.config.StateDir; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("snapshot directory is not private: %s", path)
		}
	}
	if _, err := e.CaptureRunSnapshot(context.Background(), "snapshot-owner", "snapshot-session", "run-one"); err == nil {
		t.Fatal("completed snapshot key reused")
	}
	if data, _ := os.ReadFile(filepath.Join(snapshot, "invoice.mjs")); string(data) != "independent edit" {
		t.Fatal("reuse attempt replaced existing run workspace")
	}
}

func TestCaptureRunSnapshotRefusesForeignIdentityAndUnconfirmedParent(t *testing.T) {
	for _, name := range []string{"foreign-owner", "invalid-session", "invalid-key", "missing-baseline", "writer-marker", "dangling-writer-marker", "parent-symlink"} {
		t.Run(name, func(t *testing.T) {
			e, parent := snapshotExecutor(t)
			owner, session, key := "snapshot-owner", "snapshot-session", "run-one"
			switch name {
			case "foreign-owner":
				owner = "foreign"
			case "invalid-session":
				session = "../escape"
			case "invalid-key":
				key = "../escape"
			case "missing-baseline":
				if err := os.Remove(e.metadata(parent, "baseline")); err != nil {
					t.Fatal(err)
				}
			case "writer-marker":
				if err := os.WriteFile(e.metadata(parent, "writer-active"), []byte("pending"), 0600); err != nil {
					t.Fatal(err)
				}
			case "dangling-writer-marker":
				if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), e.metadata(parent, "writer-active")); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				if err := os.Remove(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), parent); err != nil {
					t.Fatal(err)
				}
			}
			_, err := e.CaptureRunSnapshot(context.Background(), owner, session, key)
			if err == nil {
				t.Fatal("unsafe snapshot admitted")
			}
			if strings.Contains(name, "writer-marker") && !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
				t.Fatalf("writer custody reason lost: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(e.config.StateDir, "native-snapshots")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused snapshot wrote output")
			}
		})
	}
}

func TestCaptureRunSnapshotFailedCopyCannotBeReused(t *testing.T) {
	for _, kind := range []string{"symlink", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			e, parent := snapshotExecutor(t)
			bad := filepath.Join(parent, "unsafe")
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), bad); err != nil {
					t.Fatal(err)
				}
			} else {
				file, err := os.Create(bad)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate((4 << 20) + 1); err != nil {
					t.Fatal(err)
				}
				_ = file.Close()
			}
			if _, err := e.CaptureRunSnapshot(context.Background(), "snapshot-owner", "snapshot-session", "failed-run"); err == nil {
				t.Fatal("unsafe or unbounded copy succeeded")
			}
			if err := os.Remove(bad); err != nil {
				t.Fatal(err)
			}
			if _, err := e.CaptureRunSnapshot(context.Background(), "snapshot-owner", "snapshot-session", "failed-run"); err == nil {
				t.Fatal("failed key reused after source changed")
			}
			runDir := filepath.Join(e.config.StateDir, "native-snapshots", "snapshot-session", fmt.Sprintf("%x", sha256.Sum256([]byte("failed-run"))))
			entries, err := os.ReadDir(runDir)
			if err != nil || len(entries) != 0 {
				t.Fatal("partial source was published or temporary data was retained")
			}
		})
	}
}

func TestCaptureRunSnapshotReservesKeyAtomicallyAndHonorsCancellation(t *testing.T) {
	e, _ := snapshotExecutor(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.CaptureRunSnapshot(cancelled, "snapshot-owner", "snapshot-session", "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture continued: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := e.CaptureRunSnapshot(context.Background(), "snapshot-owner", "snapshot-session", "same-run")
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent key reservation produced %d snapshots", successes)
	}
}
