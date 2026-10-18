package livejournal

import (
	"context"

	"github.com/goobers/goobers/internal/blobstore"
)

type requestBlobStoreKey struct{}
type requestBlobStore struct{ store blobstore.Store }

// WithRequestBlobStore installs trusted request-local custody for synchronous
// Emit processing. Nil deliberately disables blob access; it never falls back
// to the writer's shared store. Only the daemon authority adapter should call it.
func WithRequestBlobStore(ctx context.Context, store blobstore.Store) context.Context {
	return context.WithValue(ctx, requestBlobStoreKey{}, requestBlobStore{store})
}

func (w *Writer) requestSpanSource(ctx context.Context) SpanSource {
	if bound, ok := ctx.Value(requestBlobStoreKey{}).(requestBlobStore); ok {
		return bound.store
	}
	return w.spans
}
func (w *Writer) requestArtifactSink(ctx context.Context) artifactSink {
	if bound, ok := ctx.Value(requestBlobStoreKey{}).(requestBlobStore); ok {
		return bound.store
	}
	return w.sink
}

func (w *Writer) requestArtifactSource(ctx context.Context) ArtifactSource {
	if bound, ok := ctx.Value(requestBlobStoreKey{}).(requestBlobStore); ok {
		source, _ := bound.store.(blobstore.BoundedReader)
		return source
	}
	return w.artifacts
}
