package triggerqueue

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestChildBlobCustodyIsScopedBoundedAndPruned(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	child := acceptChildTest(t, s, reservedChildRequest("parent"), childTestTime)
	data := []byte("private carrier")
	digest := "sha256:" + childDigest(data)
	before := childReservedBytes(t, s, child.ChildID)
	if err := s.KeepChildBlob(t.Context(), child.Identity, digest, data); err != nil {
		t.Fatal(err)
	}
	want := before + MaxChildBlobCustodyBytes - len(data) - 1024
	if got := childReservedBytes(t, s, child.ChildID); got != want {
		t.Fatalf("reservation=%d want%d", got, want)
	}
	if err := s.KeepChildBlob(t.Context(), child.Identity, digest, data); err != nil {
		t.Fatal(err)
	}
	if got := childReservedBytes(t, s, child.ChildID); got != want {
		t.Fatal("retry consumed reserve")
	}
	foreign := child.Identity
	foreign.Gaggle = "foreign"
	if _, err := s.ChildBlob(t.Context(), foreign, digest); !errors.Is(err, ErrChildBlobUnavailable) {
		t.Fatal("foreign read", err)
	}
	if err := s.KeepChildBlob(t.Context(), child.Identity, digest, []byte("changed")); !errors.Is(err, ErrChildBlobUnavailable) {
		t.Fatal("substitution", err)
	}
	if got, err := s.ChildBlob(t.Context(), child.Identity, digest); err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	failChildTest(t, s, child, childTestTime, true)
	if err := s.MarkChildParentSettled(t.Context(), child.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneChildren(t.Context(), childTestTime.Add(31*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if childTableCount(t, s, "child_blobs") != 0 {
		t.Fatal("pruner retained child blobs")
	}
}

func TestChildBlobCustodyReservesBeforeLaunchAndCapsGrowth(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	child := acceptChildTest(t, s, reservedChildRequest("parent"), childTestTime)
	for i := range 2 {
		data := bytes.Repeat([]byte{byte(i)}, MaxChildBlobBytes)
		if err := s.KeepChildBlob(t.Context(), child.Identity, "sha256:"+childDigest(data), data); err != nil {
			t.Fatal(err)
		}
	}
	data := bytes.Repeat([]byte{3}, MaxChildBlobBytes)
	if err := s.KeepChildBlob(t.Context(), child.Identity, "sha256:"+childDigest(data), data); !errors.Is(err, ErrFull) {
		t.Fatal("per-child byte cap", err)
	}
	if childTableCount(t, s, "child_blobs") != 2 {
		t.Fatal("failed upload changed custody")
	}
}
