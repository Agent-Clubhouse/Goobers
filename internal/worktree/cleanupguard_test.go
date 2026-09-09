package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCleanupHandoffsComposeAndStopOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		manager := &Manager{}
		var calls []int
		blocked := errors.New("first handoff failed")
		if err := manager.SetCleanupGuard("first", func(context.Context, CleanupTarget) error {
			calls = append(calls, 1)
			if fail {
				return blocked
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := manager.SetCleanupGuard("second", func(context.Context, CleanupTarget) error {
			calls = append(calls, 2)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		err := manager.prepareCleanup(context.Background(), "path", "stage", "owner")
		want := []int{1, 2}
		if fail {
			want = []int{1}
		}
		if !slices.Equal(calls, want) || errors.Is(err, blocked) != fail || errors.Is(err, ErrCleanupDeferred) != fail {
			t.Fatalf("handoff composition: calls=%v error=%v", calls, err)
		}
	}
}

func TestPreservationCapturesRecoveryWithoutRetiringReceipts(t *testing.T) {
	manager := &Manager{}
	var calls []string
	for _, name := range []string{"recovery", MutationReceiptGuard} {
		if err := manager.SetCleanupGuard(name, func(_ context.Context, target CleanupTarget) error {
			if target.OwnerRunID != "owner" || !target.Pinned || target.RepositoryDigest != "repository" {
				t.Fatalf("handoff lost custody metadata: %+v", target)
			}
			calls = append(calls, name)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	target := CleanupTarget{OwnerRunID: "owner", Pinned: true, RepositoryDigest: "repository"}
	if err := manager.preparePreservedTarget(t.Context(), target); err != nil || !slices.Equal(calls, []string{"recovery"}) {
		t.Fatalf("preservation retired receipts: %v %v", calls, err)
	}
	calls = nil
	acknowledged, err := manager.prepareCleanupTargetWithReceipts(t.Context(), target)
	if err != nil || !acknowledged || !slices.Equal(calls, []string{MutationReceiptGuard, "recovery"}) {
		t.Fatalf("destruction did not require both handoffs: %v %v %v", calls, acknowledged, err)
	}
}

func TestNamedCleanupFailurePreservesCauseAndDefers(t *testing.T) {
	manager := &Manager{}
	cause := errors.New("journal unavailable")
	if err := manager.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { return cause }); err != nil {
		t.Fatal(err)
	}
	err := manager.prepareCleanup(context.Background(), "path", "stage", "owner")
	if !errors.Is(err, ErrCleanupDeferred) || !errors.Is(err, cause) {
		t.Fatalf("cleanup lost deferred classification or original cause: %v", err)
	}
}

func TestNamedCleanupGuardReplacementPreservesOtherHandoffs(t *testing.T) {
	manager := &Manager{}
	var calls []string
	for _, name := range []string{"original", "provenance", "recovery", "recovery"} {
		if err := manager.SetCleanupGuard(name, func(context.Context, CleanupTarget) error { calls = append(calls, name); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.prepareCleanup(context.Background(), "path", "stage", "owner"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"original", "provenance", "recovery"}) {
		t.Fatalf("reload replaced unrelated guards or duplicated recovery: %v", calls)
	}
}

func TestNamedCleanupGuardConcurrentReplacement(t *testing.T) {
	manager := &Manager{}
	var calls atomic.Int64
	callback := func(context.Context, CleanupTarget) error { calls.Add(1); return nil }
	if err := manager.SetCleanupGuard("recovery", callback); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 100 {
			if err := manager.SetCleanupGuard("recovery", callback); err != nil {
				t.Error(err)
			}
		}
	})
	workers.Go(func() {
		for range 100 {
			if err := manager.prepareCleanup(context.Background(), "path", "stage", "owner"); err != nil {
				t.Error(err)
			}
		}
	})
	workers.Wait()
	if calls.Load() != 100 {
		t.Fatalf("reload duplicated or lost cleanup acknowledgements: %d", calls.Load())
	}
}

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
