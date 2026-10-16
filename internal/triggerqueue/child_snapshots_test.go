package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
)

func childSnapshotTestValue(receipt, bundle string) ChildSnapshot {
	return ChildSnapshot{Receipt: []byte(receipt), ReceiptDigest: "sha256:" + childDigest([]byte(receipt)), Bundle: []byte(bundle), BundleDigest: "sha256:" + childDigest([]byte(bundle))}
}

func TestChildSnapshotCustodySurvivesReopenAndIsImmutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	store := openTestStore(t, path)
	child := acceptChildTest(t, store, childRequest("parent", "stage", "key"), childTestTime)
	if _, err := store.ChildSnapshot(t.Context(), child.Identity); !errors.Is(err, ErrChildSnapshotPending) {
		t.Fatalf("uncaptured snapshot=%v", err)
	}
	snapshot := childSnapshotTestValue("receipt", "bundle")
	if err := store.KeepChildSnapshot(t.Context(), child, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	got, err := store.ChildSnapshot(t.Context(), child.Identity)
	if err != nil || string(got.Bundle) != "bundle" {
		t.Fatalf("retained=%+v %v", got, err)
	}
	if err := store.KeepChildSnapshot(t.Context(), child, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.KeepChildSnapshot(t.Context(), child, childSnapshotTestValue("changed", "bundle")); !errors.Is(err, ErrConflict) {
		t.Fatalf("recaptured parent accepted: %v", err)
	}
	child.AcceptanceID = "foreign"
	if err := store.KeepChildSnapshot(t.Context(), child, snapshot); !errors.Is(err, ErrChildSnapshotUnavailable) {
		t.Fatalf("foreign acceptance=%v", err)
	}
}

func TestChildSnapshotLossIsNotRecapturePermission(t *testing.T) {
	for _, query := range []string{`DELETE FROM child_snapshots`, `UPDATE child_snapshots SET bundle=x'626164'`, `UPDATE child_snapshots SET receipt=x'626164'`} {
		t.Run(query, func(t *testing.T) {
			store := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
			child := acceptChildTest(t, store, childRequest("parent", "stage", "key"), childTestTime)
			snapshot := childSnapshotTestValue("receipt", "bundle")
			if err := store.KeepChildSnapshot(t.Context(), child, snapshot); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ChildSnapshot(t.Context(), child.Identity); !errors.Is(err, ErrChildSnapshotUnavailable) {
				t.Fatalf("lost custody=%v", err)
			}
			if err := store.KeepChildSnapshot(t.Context(), child, snapshot); !errors.Is(err, ErrChildSnapshotUnavailable) {
				t.Fatalf("silently repaired lost custody=%v", err)
			}
		})
	}
}

func TestChildSnapshotBoundsCancellationAndRetention(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	child := acceptChildTest(t, store, childRequest("parent", "stage", "key"), childTestTime)
	invalid := childSnapshotTestValue("receipt", "bundle")
	invalid.Bundle = make([]byte, MaxChildSnapshotBytes+1)
	if err := store.KeepChildSnapshot(t.Context(), child, invalid); !errors.Is(err, ErrChildSnapshotUnavailable) {
		t.Fatalf("unbounded capture=%v", err)
	}
	snapshot := childSnapshotTestValue("receipt", "bundle")
	if err := store.KeepChildSnapshot(t.Context(), child, snapshot); err != nil {
		t.Fatal(err)
	}
	if result, err := store.PruneChildren(t.Context(), childTestTime.Add(100*ChildRetention), 100); err != nil || result.total() != 0 {
		t.Fatalf("unresolved capture pruned: %+v %v", result, err)
	}
	failChildTest(t, store, child, childTestTime, true)
	if err := store.MarkChildParentSettled(t.Context(), child.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	if result, err := store.PruneChildren(t.Context(), childTestTime.Add(ChildRetention), 100); err != nil || result.Tombstoned != 1 {
		t.Fatalf("retention=%+v %v", result, err)
	}
	if n := childTableCount(t, store, "child_snapshots"); n != 0 {
		t.Fatalf("pruner left %d carrier blobs", n)
	}
	if _, err := store.ChildSnapshot(t.Context(), child.Identity); !errors.Is(err, ErrChildSnapshotUnavailable) {
		t.Fatalf("tombstoned read=%v", err)
	}
	cancelled := acceptChildTest(t, store, childRequest("cancelled", "stage", "key"), childTestTime)
	if err := store.FenceChildParent(t.Context(), cancelled.Identity.ChildParent, "operator", childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := store.KeepChildSnapshot(t.Context(), cancelled, snapshot); !errors.Is(err, ErrParentCancelled) {
		t.Fatalf("cancelled capture=%v", err)
	}
}
