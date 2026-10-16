package triggerqueue

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func childResultValue(receipt, bundle string) ChildResult {
	r := ChildResult{Receipt: []byte(receipt), ReceiptDigest: "sha256:" + childDigest([]byte(receipt)), Bundle: []byte(bundle)}
	if bundle != "" {
		r.BundleDigest = "sha256:" + childDigest([]byte(bundle))
	}
	return r
}

func TestChildResultFirstCaptureSurvivesRestartAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	store := openTestStore(t, path)
	child := acceptChildTest(t, store, childRequest("parent", "stage", "key"), childTestTime)
	if _, err := store.ChildResult(t.Context(), child.Identity); !errors.Is(err, ErrChildResultPending) {
		t.Fatal(err)
	}
	if err := store.FenceChildParent(t.Context(), child.Identity.ChildParent, "operator", childTestTime); err != nil {
		t.Fatal(err)
	}
	result := childResultValue("terminal cancellation", "workspace")
	if err := store.KeepChildResult(t.Context(), child, result); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	if err := store.KeepChildResult(t.Context(), child, result); err != nil {
		t.Fatal(err)
	}
	if err := store.KeepChildResult(t.Context(), child, childResultValue("replacement", "workspace")); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	got, err := store.ChildResult(t.Context(), child.Identity)
	if err != nil || string(got.Bundle) != "workspace" {
		t.Fatalf("%+v %v", got, err)
	}
	child.AcceptanceID = "foreign"
	if err := store.KeepChildResult(t.Context(), child, result); !errors.Is(err, ErrChildResultUnavailable) {
		t.Fatal(err)
	}
}

func TestChildResultLossAndBounds(t *testing.T) {
	for _, query := range []string{`DELETE FROM child_results`, `UPDATE child_results SET receipt=x'626164'`, `UPDATE child_results SET bundle=x'626164'`} {
		t.Run(query, func(t *testing.T) {
			store := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
			child := acceptChildTest(t, store, childRequest("parent", "stage", "key"), childTestTime)
			result := childResultValue("receipt", "bundle")
			if err := store.KeepChildResult(t.Context(), child, result); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ChildResult(t.Context(), child.Identity); !errors.Is(err, ErrChildResultUnavailable) {
				t.Fatal(err)
			}
			if err := store.KeepChildResult(t.Context(), child, result); !errors.Is(err, ErrChildResultUnavailable) {
				t.Fatal(err)
			}
		})
	}
	for _, result := range []ChildResult{childResultValue(string(make([]byte, MaxChildSnapshotReceiptBytes+1)), ""), childResultValue("receipt", string(make([]byte, MaxChildSnapshotBytes+1)))} {
		if err := result.validate(); !errors.Is(err, ErrChildResultUnavailable) {
			t.Fatal(err)
		}
	}
}

func TestChildResultFamilyRetentionReachesSteadyState(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	for i := range 12 {
		now := childTestTime.Add(time.Duration(i) * ChildRetention * 3)
		child := acceptChildTest(t, store, childRequest("parent-"+strconv.Itoa(i), "stage", "key"), now)
		result := childResultValue("terminal receipt", "")
		if err := store.KeepChildResult(t.Context(), child, result); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PruneChildren(t.Context(), now.Add(ChildRetention), 100); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ChildResult(t.Context(), child.Identity); err != nil {
			t.Fatal("unsettled result pruned", err)
		}
		if err := store.SetChildState(t.Context(), child.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: result.ReceiptDigest}, now); err != nil {
			t.Fatal(err)
		}
		if err := store.AcknowledgeChild(t.Context(), child.Identity, result.ReceiptDigest, now); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkChildParentSettled(t.Context(), child.Identity.ChildParent, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PruneChildren(t.Context(), now.Add(ChildRetention), 100); err != nil {
			t.Fatal(err)
		}
		if count := childTableCount(t, store, "child_results"); count != 0 {
			t.Fatalf("result carriers grew to %d", count)
		}
	}
}
