package childpod

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestBlobOverlayNeverFallsThroughForChildReadsOrWrites(t *testing.T) {
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	base, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("shared private payload")
	digest := journal.Digest(data)
	if err = base.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	child := true
	store := BlobOverlay{Base: base, Queue: queue, Scope: func(context.Context) (triggerqueue.ChildIdentity, bool, error) {
		return triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "gaggle", ParentRunID: "parent"}, StageOccurrence: "stage", InvocationKey: "key"}, child, nil
	}}
	if _, err = store.Get(t.Context(), digest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("foreign fallback", err)
	}
	newData := []byte("unknown child upload")
	newDigest := journal.Digest(newData)
	if err = store.Put(t.Context(), newDigest, newData); err == nil {
		t.Fatal("unknown child upload accepted")
	}
	if ok, err := base.Has(t.Context(), newDigest); err != nil || ok {
		t.Fatal("child PUT escaped queue", err)
	}
	child = false
	if got, err := store.Get(t.Context(), digest); err != nil || string(got) != string(data) {
		t.Fatal("ordinary read changed", err)
	}
	store.Scope = func(context.Context) (triggerqueue.ChildIdentity, bool, error) {
		return triggerqueue.ChildIdentity{}, false, errors.New("revoked")
	}
	if _, err = store.Get(t.Context(), digest); err == nil {
		t.Fatal("revoked request fell back")
	}
}
