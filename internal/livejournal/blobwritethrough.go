package livejournal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
)

// artifactSink is the write half of the daemon's blob plane (satisfied by
// internal/blobstore.Store): the store a mode-3 stage pod resolves every
// upstream context pointer through (materializePodContext), and nothing else.
//
// It is not a separate option. The daemon hands its one blob store to
// WithSpanSource, and that store is also the one the blob plane serves; a
// span source that can Put is therefore taken as the sink too, so the
// daemon's existing wiring enables the write-through with no second knob to
// forget (#5550). A source that only reads — every test fake, and any
// assembly without a store — leaves the write-through off, byte-for-byte as
// before.
type artifactSink interface {
	Put(ctx context.Context, digest string, data []byte) error
}

// publishArtifact writes a committed artifact's stored bytes through to the
// blob plane under its committed digest (#5550). Caller holds run.mu.
//
// Why: an artifact the WORKFLOW authors — a gate verdict the engine
// re-marshals, a context manifest — reaches this daemon only as an emitted op.
// Before this, it landed in the run journal and nowhere else, while the engine
// injects a pointer to it into the next dispatch and a pod resolves pointers
// only through the blob plane. The next pod stage then died on
// context_materialize_failed ("upstream context artifact is not in the blob
// plane"). Only pod-authored artifacts had been written through, by the pod.
//
// The bytes are read back from the journal's own content-addressed file, not
// taken from the op: the journal scrubs before it stores, and the blob plane
// must hold exactly what the journal committed. They are verified to hash to
// the committed digest before the Put — the store keys by digest and does not
// re-derive it, so a mismatch published here would be served as the wrong
// content forever.
func (w *Writer) publishArtifact(ctx context.Context, run *liveRun, ref journal.Ref) error {
	if w.sink == nil {
		return nil
	}
	rel, err := journal.ArtifactPath(ref.Digest)
	if err != nil || rel != ref.Path {
		return fmt.Errorf("publish artifact %s: committed ref is not content-addressed", ref.Digest)
	}
	data, err := os.ReadFile(filepath.Join(run.jr.Dir(), filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("publish artifact %s: read committed bytes: %w", ref.Digest, err)
	}
	if got := journal.Digest(data); got != ref.Digest || int64(len(data)) != ref.Size {
		return fmt.Errorf("publish artifact %s: committed bytes hash to %s (%d bytes, want %d)", ref.Digest, got, len(data), ref.Size)
	}
	if err := w.sink.Put(ctx, ref.Digest, data); err != nil {
		return fmt.Errorf("publish artifact %s to the blob plane: %w", ref.Digest, err)
	}
	return nil
}

// republishArtifact is the retry half of publishArtifact, for an artifact op
// that arrives already applied. A failed Put fails the emit AFTER the journal
// committed the artifact (the journal is the record and cannot un-append), so
// the retried op is deduplicated — and without this, deduplication would be
// exactly how "in the journal" and "in the blob plane" came apart for good.
// Put is idempotent by digest, so re-publishing an already-present blob is a
// no-op. Caller holds run.mu.
func (w *Writer) republishArtifact(ctx context.Context, run *liveRun, op Op) error {
	if w.sink == nil || op.Kind != OpArtifact || run.jr == nil {
		return nil
	}
	ref, ok := run.artifactKeyRefs[op.Key]
	if !ok {
		return nil
	}
	return w.publishArtifact(ctx, run, ref)
}

// rememberArtifactKey records which committed ref an artifact op's key
// produced, so a deduplicated retry can re-publish it. Lazily allocated: the
// map is only consulted when a sink is configured.
func (run *liveRun) rememberArtifactKey(key string, ref journal.Ref) {
	if key == "" {
		return
	}
	if run.artifactKeyRefs == nil {
		run.artifactKeyRefs = map[string]journal.Ref{}
	}
	run.artifactKeyRefs[key] = ref
}
