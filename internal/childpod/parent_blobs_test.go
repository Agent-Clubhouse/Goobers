package childpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

func parentStore(t *testing.T) ParentBlobs {
	t.Helper()
	id := requestFixture().Identity
	id.Child = nil
	run, err := journal.Create(t.TempDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return ParentBlobs{RunDir: run.Dir(), Identity: id}
}

func TestParentBlobsPinOwnerAndExpireWithRun(t *testing.T) {
	s := parentStore(t)
	data := []byte("private parent custody")
	digest := journal.Digest(data)
	if err := s.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(t.Context(), digest); err != nil || string(got) != string(data) {
		t.Fatal(err)
	}
	wrong := s
	wrong.Identity.Gaggle = "foreign"
	if _, err := wrong.Get(t.Context(), digest); err == nil {
		t.Fatal("foreign owner read")
	}
	if err := wrong.Put(t.Context(), digest, data); err == nil {
		t.Fatal("foreign owner write")
	}
	if err := os.RemoveAll(s.RunDir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(t.Context(), digest); err == nil {
		t.Fatal("pruned journal still owns bytes")
	}
	if err := s.Put(t.Context(), digest, data); err == nil {
		t.Fatal("write resurrected pruned run")
	}
}

func TestParentBlobQuotaAndConcurrentDuplicate(t *testing.T) {
	s := parentStore(t)
	ctx := t.Context()
	data := []byte("same durable payload")
	digest := journal.Digest(data)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := s.Put(ctx, digest, data); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	root, err := os.OpenRoot(filepath.Join(s.RunDir, parentCustodyDirectory))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	count, _, err := parentCustodySize(root)
	if err != nil || count != 1 {
		t.Fatal(count, err)
	}
	// Sparse local fixture exercises quota without allocating a huge body.
	f, err := os.Create(filepath.Join(root.Name(), "quota-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(MaxParentCustodyBytes); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	next := []byte("new")
	if err = s.Put(ctx, journal.Digest(next), next); err == nil {
		t.Fatal("total quota exceeded")
	}
	if err = s.Put(ctx, digest, data); err != nil {
		t.Fatal("quota blocked idempotent existing bytes", err)
	}
	if err = os.Remove(filepath.Join(root.Name(), "quota-fixture")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < MaxParentCustodyBlobs; i++ {
		if err = os.WriteFile(filepath.Join(root.Name(), fmt.Sprint(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Put(ctx, journal.Digest(next), next); err == nil {
		t.Fatal("entry quota exceeded")
	}
}

func TestParentBlobRejectsMissingAndOversizedBodies(t *testing.T) {
	s := parentStore(t)
	if _, err := s.Get(t.Context(), journal.Digest([]byte("missing"))); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal(err)
	}
	data := make([]byte, MaxContractBytes+1)
	if err := s.Put(t.Context(), journal.Digest(data), data); err == nil {
		t.Fatal("oversized body accepted")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Put(canceled, journal.Digest(nil), nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestParentAttemptScopeCannotReadSiblingOrGrantHashOnlyAccess(t *testing.T) {
	s := parentStore(t)
	contract := Contract{Version: 1, Identity: s.Identity, ParentOrigin: &apiv1.ChildWorkflowOrigin{StageOccurrence: "occurrence", AttemptID: "attempt"}, Stage: "stage", Attempt: 1, PodAttempt: 1, StartedAt: s.Identity.StartedAt, Ceiling: requestFixture().Ceiling}
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(data)
	if err = s.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	if err = s.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	scope := ParentAttemptBlobs{Store: s, ContractDigest: digest}
	if _, err = scope.Get(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	secret := []byte("sibling private custody")
	secretDigest := journal.Digest(secret)
	if err = s.Put(t.Context(), secretDigest, secret); err != nil {
		t.Fatal(err)
	}
	if _, err = scope.Get(t.Context(), secretDigest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("sibling read", err)
	}
	if err = scope.Put(t.Context(), secretDigest, nil); err == nil {
		t.Fatal("hash-only ownership")
	}
	if _, err = scope.Get(t.Context(), secretDigest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("failed upload granted read", err)
	}
	own := []byte("own transcript")
	ownDigest := journal.Digest(own)
	if err = scope.Put(t.Context(), ownDigest, own); err != nil {
		t.Fatal(err)
	}
	if _, err = scope.Get(t.Context(), ownDigest); err != nil {
		t.Fatal(err)
	}
	contract.PodAttempt = 2
	data, _ = json.Marshal(contract)
	other := journal.Digest(data)
	if err = s.Put(t.Context(), other, data); err != nil {
		t.Fatal(err)
	}
	if err = s.BindContract(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	sibling := ParentAttemptBlobs{Store: s, ContractDigest: other}
	if _, err = sibling.Get(t.Context(), ownDigest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("later attempt read earlier transcript without declared context", err)
	}
}

func TestParentBoundedReadsKeepAttemptScope(t *testing.T) {
	s := parentStore(t)
	data := []byte("bounded custody")
	digest := journal.Digest(data)
	contract := Contract{Version: 1, Identity: s.Identity, ParentOrigin: &apiv1.ChildWorkflowOrigin{StageOccurrence: "occurrence", AttemptID: "attempt"}, Stage: "stage", Attempt: 1, PodAttempt: 1, StartedAt: s.Identity.StartedAt, Ceiling: requestFixture().Ceiling}
	contractData, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	contractDigest := journal.Digest(contractData)
	if err = s.Put(t.Context(), contractDigest, contractData); err != nil {
		t.Fatal(err)
	}
	attempt := ParentAttemptBlobs{Store: s, ContractDigest: contractDigest}
	if err := attempt.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	for _, store := range []blobstore.BoundedReader{s, attempt} {
		if got, err := store.GetBounded(t.Context(), digest, int64(len(data)-1)); got != nil || !errors.Is(err, blobstore.ErrTooLarge) {
			t.Fatal(got, err)
		}
		if got, err := store.GetBounded(t.Context(), digest, int64(len(data))); err != nil || string(got) != string(data) {
			t.Fatal(got, err)
		}
	}
	attempt.ContractDigest = journal.Digest([]byte("sibling"))
	if _, err := attempt.GetBounded(t.Context(), digest, 100); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal(err)
	}
}
