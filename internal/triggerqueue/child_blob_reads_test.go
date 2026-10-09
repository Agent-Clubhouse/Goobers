package triggerqueue

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestChildAttemptBlobIsolationReopenAndPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s := openTestStore(t, path)
	child := acceptChildTest(t, s, reservedChildRequest("parent"), childTestTime)
	keep := func(data []byte) string {
		digest := "sha256:" + childDigest(data)
		if err := s.KeepChildBlob(t.Context(), child.Identity, digest, data); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	a, b := keep([]byte("contract-a")), keep([]byte("contract-b"))
	secret := []byte("attempt-a-private")
	digest := "sha256:" + childDigest(secret)
	if err := s.KeepChildAttemptBlob(t.Context(), child.Identity, a, digest, secret); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ChildAttemptBlobBounded(t.Context(), child.Identity, b, digest, 1024); got != nil || !errors.Is(err, ErrChildBlobUnavailable) {
		t.Fatal("sibling read", got, err)
	}
	before := childReservedBytes(t, s, child.ChildID)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := s.KeepChildAttemptBlob(t.Context(), child.Identity, a, digest, secret); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if after := childReservedBytes(t, s, child.ChildID); after != before {
		t.Fatal("duplicate membership consumed custody", before, after)
	}
	failedData := []byte("must-roll-back")
	failedDigest := "sha256:" + childDigest(failedData)
	if err := s.KeepChildAttemptBlob(t.Context(), child.Identity, "sha256:"+childDigest([]byte("missing")), failedDigest, failedData); err == nil {
		t.Fatal("missing contract accepted")
	}
	if _, err := s.ChildBlob(t.Context(), child.Identity, failedDigest); !errors.Is(err, ErrChildBlobUnavailable) {
		t.Fatal("failed ownership left orphan bytes", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	if got, err := s.ChildAttemptBlobBounded(t.Context(), child.Identity, a, digest, int64(len(secret))); err != nil || string(got) != string(secret) {
		t.Fatal(string(got), err)
	}
	if got, err := s.ChildAttemptBlobBounded(t.Context(), child.Identity, a, digest, int64(len(secret)-1)); err == nil || got != nil {
		t.Fatal("bounded attempt read ignored caller", err)
	}
	failChildTest(t, s, child, childTestTime, true)
	if err := s.MarkChildParentSettled(t.Context(), child.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneChildren(t.Context(), childTestTime.Add(31*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if childTableCount(t, s, "child_blob_reads") != 0 {
		t.Fatal("production child pruner retained read authority")
	}
}

func TestChildAttemptMembershipSharesBlobCountAndByteQuota(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	child := acceptChildTest(t, s, reservedChildRequest("parent"), childTestTime)
	contract := []byte("contract")
	digest := "sha256:" + childDigest(contract)
	if err := s.KeepChildBlob(t.Context(), child.Identity, digest, contract); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxChildBlobs-1; i++ {
		// Simulate prior independently bounded owner rows; the public mutation
		// still must include them in its quota before storing the next blob.
		if _, err := s.db.Exec(`INSERT INTO child_blob_reads(child_id,contract_digest,digest) VALUES(?,?,?)`, child.ChildID, digest, string(rune(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("next")
	if err := s.KeepChildBlob(t.Context(), child.Identity, "sha256:"+childDigest(data), data); !errors.Is(err, ErrFull) {
		t.Fatal("read index escaped shared row cap", err)
	}
}
