package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupGuardPreservesEvidenceOnEveryDestructivePath(t *testing.T) {
	for _, mode := range []string{"teardown", "orphan-or-retention", "stale-replacement"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			blocked := errors.New("journal persistence failed")
			deny := true
			var targets []CleanupTarget
			m, err := NewManager(t.TempDir(), WithMutationReceiptCleanup(func(_ context.Context, target CleanupTarget) error {
				targets = append(targets, target)
				if deny {
					return blocked
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			wt, err := m.Create(ctx, CreateOptions{RepoURL: newSourceRepo(t), RunID: "owner-with-hyphens-stage", OwnerRunID: "owner-with-hyphens", BaseRef: "main"})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(wt.Path, "uncommitted-evidence.json")
			if err := os.WriteFile(path, []byte("durable handoff pending"), 0600); err != nil {
				t.Fatal(err)
			}
			markerPath := m.markerPath(wt.key, wt.RunID)
			mk, err := readMarker(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			cleanup := func() error {
				switch mode {
				case "teardown":
					return wt.Remove(ctx, RemoveOptions{})
				case "orphan-or-retention":
					return m.reapOne(ctx, wt.key, wt.Path, markerPath, &mk)
				default:
					return m.forceClear(ctx, wt.key, wt.Path, wt.RunID)
				}
			}
			if err := cleanup(); !errors.Is(err, blocked) {
				t.Fatalf("cleanup ignored handoff failure: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "durable handoff pending" {
				t.Fatalf("lost evidence: %q %v", data, err)
			}
			if _, err := readMarker(markerPath); err != nil {
				t.Fatalf("lost owner marker: %v", err)
			}
			if len(targets) != 1 || targets[0].Path != wt.Path || targets[0].OwnerRunID != "owner-with-hyphens" || targets[0].WorktreeID != wt.RunID {
				t.Fatalf("wrong cleanup identity: %+v", targets)
			}
			deny = false
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
				t.Fatalf("acknowledged cleanup retained directory: %v", err)
			}
		})
	}
}

func TestKeepingWorktreeDoesNotInvokeCleanupGuard(t *testing.T) {
	ctx := context.Background()
	m, err := NewManager(t.TempDir(), WithMutationReceiptCleanup(func(context.Context, CleanupTarget) error {
		t.Error("guard called without deletion")
		return errors.New("unexpected cleanup")
	}))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateOptions{RepoURL: newSourceRepo(t), RunID: "kept", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, RemoveOptions{Keep: true}); err != nil {
		t.Fatal(err)
	}
}
