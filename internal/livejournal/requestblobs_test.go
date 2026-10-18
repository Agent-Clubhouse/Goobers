package livejournal

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestRequestBlobCustodyOverridesReadsWritesAndRetry(t *testing.T) {
	base, owned := newBlobDir(t), newBlobDir(t)
	w, runs := testWriter(t, WithSpanSource(base), WithArtifactSource(base))
	now := time.Now().UTC()
	if _, err := w.Emit(t.Context(), openBatch("run-custody", now)); err != nil {
		t.Fatal(err)
	}
	data := []byte("owned observation")
	digest := journal.Digest(data)
	flaky := &flakyStore{Dir: owned, failPuts: 1}
	ctx := WithRequestBlobStore(t.Context(), flaky)
	req := verdictOp("run-custody", now.Add(time.Second), data)
	if _, err := w.Emit(ctx, req); err == nil {
		t.Fatal("failed scoped publication hidden")
	}
	out, err := w.Emit(ctx, req)
	if err != nil || out.Deduplicated != 1 {
		t.Fatal(out, err)
	}
	if found, _ := base.Has(t.Context(), digest); found {
		t.Fatal("private data reached shared sink")
	}
	if got, err := owned.Get(t.Context(), digest); err != nil || string(got) != string(data) {
		t.Fatal(got, err)
	}
	if len(recordedArtifacts(t, runs, "run-custody")) != 1 {
		t.Fatal("retry duplicated artifact")
	}
	shared := []byte("shared secret")
	sharedDigest := journal.Digest(shared)
	if err := base.Put(t.Context(), sharedDigest, shared); err != nil {
		t.Fatal(err)
	}
	if _, err := w.fetchSpan(ctx, sharedDigest); err == nil {
		t.Fatal("private read fell back to shared source")
	}
	if _, err := w.fetchSpan(ctx, digest); err != nil {
		t.Fatal(err)
	}
	ref, _ := journal.ArtifactRef(shared)
	if _, err := w.fetchArtifact(ctx, &ArtifactOp{Name: "foreign", Ref: &ref}, "derived"); err == nil {
		t.Fatal("artifact ref fell back to shared source")
	}
	ref, _ = journal.ArtifactRef(data)
	if _, err := w.fetchArtifact(ctx, &ArtifactOp{Name: "owned", Ref: &ref}, "derived"); err != nil {
		t.Fatal(err)
	}
	ref.Size--
	if _, err := w.fetchArtifact(ctx, &ArtifactOp{Name: "oversized", Ref: &ref}, "derived"); err == nil {
		t.Fatal("artifact caller limit ignored")
	}

	if _, err := w.fetchSpan(WithRequestBlobStore(context.Background(), nil), sharedDigest); err == nil {
		t.Fatal("explicitly absent custody fell back")
	}
	if _, err := w.fetchSpan(context.Background(), sharedDigest); err != nil {
		t.Fatal("ordinary source changed", err)
	}
}
