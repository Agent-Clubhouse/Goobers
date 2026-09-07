package artifactset

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
)

// StoreReader reads only this invocation's current-run upstream pointers from a
// bounded fleet store. It performs no cache or workspace writes. Digest knowledge
// alone does not authorize fetching another run's blob.
type StoreReader struct {
	store   blobstore.BoundedReader
	allowed map[apiv1.ArtifactPointer]bool
}

// NewStoreReader binds the runner-authored context once, before a check starts.
func NewStoreReader(store blobstore.BoundedReader, pointers []apiv1.ContextPointer) (*StoreReader, error) {
	if store == nil {
		return nil, errors.New("artifact set: bounded store reader is not configured")
	}
	if len(pointers) > 4096 {
		return nil, fmt.Errorf("%w: context pointer limit", ErrInvalid)
	}
	r := &StoreReader{store: store, allowed: make(map[apiv1.ArtifactPointer]bool)}
	for _, pointer := range pointers {
		if pointer.Artifact == nil || pointer.External != nil || pointer.RunID != "" {
			continue
		}
		if err := boundedPointer(*pointer.Artifact, MaxPayloadBytes); err != nil {
			return nil, err
		}
		r.allowed[*pointer.Artifact] = true
	}
	return r, nil
}

// Close is a no-op: this reader borrows the shared store, never closes it.
func (*StoreReader) Close() error { return nil }

// ReadArtifact fetches only an exact authorized pointer, with pre-fetch and
// post-fetch bounds, exact size, and digest verification.
func (r *StoreReader) ReadArtifact(ctx context.Context, pointer apiv1.ArtifactPointer, limit int64) ([]byte, error) {
	if !r.allowed[pointer] {
		return nil, fmt.Errorf("%w: pointer is not current-run upstream evidence", ErrInvalid)
	}
	if limit <= 0 || limit > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: invalid byte limit", ErrInvalid)
	}
	if err := boundedPointer(pointer, limit); err != nil {
		return nil, err
	}
	data, err := r.store.GetBounded(ctx, pointer.Digest, limit)
	if errors.Is(err, blobstore.ErrNotFound) || errors.Is(err, blobstore.ErrTooLarge) {
		return nil, fmt.Errorf("%w: absent, corrupt, or oversized blob", ErrInvalid)
	}
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != pointer.Size || int64(len(data)) > limit || apiv1.Digest(data) != pointer.Digest {
		return nil, fmt.Errorf("%w: store payload does not match pointer", ErrInvalid)
	}
	return data, nil
}
