package childpod

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ScopedBlobs confines all artifact custody to an accepted child's bounded,
// retained queue lineage. A missing digest never grants shared-store access.
type ScopedBlobs struct {
	Queue    *triggerqueue.Store
	Identity triggerqueue.ChildIdentity
}

// Describe names the credential-free custody backend.
func (s ScopedBlobs) Describe() string { return "bounded-child-custody" }

// Get reads only the selected authenticated custody scope.
func (s ScopedBlobs) Get(ctx context.Context, digest string) ([]byte, error) {
	if s.Queue == nil {
		return nil, fmt.Errorf("child blob queue unavailable")
	}
	data, err := s.Queue.ChildBlob(ctx, s.Identity, digest)
	if errors.Is(err, triggerqueue.ErrChildBlobUnavailable) {
		return nil, blobstore.ErrNotFound
	}
	return data, err
}

// Put retains bounded bytes only in the selected owner scope.
func (s ScopedBlobs) Put(ctx context.Context, digest string, data []byte) error {
	if s.Queue == nil {
		return fmt.Errorf("child blob queue unavailable")
	}
	return s.Queue.KeepChildBlob(ctx, s.Identity, digest, data)
}

// Has reports verified presence within the selected scope.
func (s ScopedBlobs) Has(ctx context.Context, digest string) (bool, error) {
	_, err := s.Get(ctx, digest)
	if errors.Is(err, blobstore.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// BlobOverlay selects child custody from the authenticated request principal.
// Scope must fail closed for missing/revoked child identity. A nonchild caller
// sees only Base; a child caller never falls through to it, for GET or PUT.
type BlobOverlay struct {
	Base  blobstore.Store
	Queue *triggerqueue.Store
	Scope func(context.Context) (triggerqueue.ChildIdentity, bool, error)
}

// Describe names the credential-free custody backend.
func (s BlobOverlay) Describe() string { return "authenticated-child-blob-overlay" }
func (s BlobOverlay) selectStore(ctx context.Context) (blobstore.Store, error) {
	if s.Scope == nil || s.Base == nil || s.Queue == nil {
		return nil, fmt.Errorf("child blob scope unavailable")
	}
	id, child, err := s.Scope(ctx)
	if err != nil {
		return nil, err
	}
	if child {
		return ScopedBlobs{Queue: s.Queue, Identity: id}, nil
	}
	return s.Base, nil
}

// Get reads only the selected authenticated custody scope.
func (s BlobOverlay) Get(ctx context.Context, digest string) ([]byte, error) {
	store, err := s.selectStore(ctx)
	if err != nil {
		return nil, err
	}
	return store.Get(ctx, digest)
}

// Put retains bounded bytes only in the selected owner scope.
func (s BlobOverlay) Put(ctx context.Context, digest string, data []byte) error {
	store, err := s.selectStore(ctx)
	if err != nil {
		return err
	}
	return store.Put(ctx, digest, data)
}

// Has reports verified presence within the selected scope.
func (s BlobOverlay) Has(ctx context.Context, digest string) (bool, error) {
	store, err := s.selectStore(ctx)
	if err != nil {
		return false, err
	}
	return store.Has(ctx, digest)
}
