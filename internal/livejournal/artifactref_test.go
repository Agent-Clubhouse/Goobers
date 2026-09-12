package livejournal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

type artifactSourceFunc func(context.Context, string, int64) ([]byte, error)

func (f artifactSourceFunc) GetBounded(ctx context.Context, digest string, limit int64) ([]byte, error) {
	return f(ctx, digest, limit)
}

func artifactRefRequest(t *testing.T, data []byte) EmitRequest {
	t.Helper()
	ref, err := journal.ArtifactRef(data)
	if err != nil {
		t.Fatal(err)
	}
	return EmitRequest{RunID: "ref-run", Gaggle: "web", Open: openHeader("ref-run"), Ops: []Op{{
		Kind: OpArtifact, Key: "pod/3/build/result/" + ref.Digest, Time: time.Now().UTC(),
		Artifact: &ArtifactOp{Stage: "build", Attempt: 2, Class: "infra", Name: "result", Ref: &ref},
	}}}
}

func newArtifactBlobDir(t *testing.T) *blobstore.Dir {
	t.Helper()
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testArtifactWriter(t *testing.T, opts ...Option) (*Writer, string) {
	t.Helper()
	w, runsDir := testWriter(t, opts...)
	run, err := journal.Create(runsDir, openHeader("ref-run").Identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return w, runsDir
}

func TestArtifactRefAdoptsBoundedBytesAndDeduplicatesAfterRestart(t *testing.T) {
	for _, size := range []int{0, 32, int(MaxArtifactRefBytes)} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ctx := context.Background()
			data := bytes.Repeat([]byte("a"), size)
			req := artifactRefRequest(t, data)
			req.Ops[0].Artifact.Ref.Integrity = apiv1.IntegrityMaintainer
			req.Ops[0].Artifact.Ref.MediaType = "application/octet-stream"
			store := newArtifactBlobDir(t)
			if err := store.Put(ctx, req.Ops[0].Artifact.Ref.Digest, data); err != nil {
				t.Fatal(err)
			}
			reads := 0
			source := artifactSourceFunc(func(ctx context.Context, digest string, limit int64) ([]byte, error) {
				reads++
				if limit != max(int64(size), 1) {
					t.Errorf("read bound=%d, want %d", limit, max(size, 1))
				}
				return store.GetBounded(ctx, digest, limit)
			})
			w, runsDir := testArtifactWriter(t, WithArtifactSource(source), WithScrubber(journal.Chain()))
			out, err := w.Emit(ctx, req)
			if err != nil || out.Applied != 1 {
				t.Fatalf("adopt=%+v, err=%v", out, err)
			}
			w.Close()
			w, err = NewWriter(func(string) (string, bool) { return runsDir, true }, WithArtifactSource(source))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(w.Close)
			out, err = w.Emit(ctx, req)
			if err != nil || out.Deduplicated != 1 || reads != 1 {
				t.Fatalf("redelivery=%+v, reads=%d, err=%v", out, reads, err)
			}
			events := readEvents(t, runsDir, req.RunID)
			if len(events) != 2 {
				t.Fatalf("events=%d, want run.started and one artifact", len(events))
			}
			ev := events[1]
			if ev.Stage != "build" || ev.Attempt != 2 || ev.AttemptClass != "infra" || ev.Integrity != apiv1.IntegrityMaintainer || ev.Ref == nil || *ev.Ref != *req.Ops[0].Artifact.Ref {
				t.Fatalf("artifact lineage/ref changed: %+v", ev)
			}
			reader, err := journal.OpenRead(filepath.Join(runsDir, req.RunID))
			if err != nil {
				t.Fatal(err)
			}
			stored, err := reader.ArtifactBytesBounded(*ev.Ref, max(int64(size), 1))
			if err != nil || !bytes.Equal(stored, data) {
				t.Fatalf("stored evidence mismatch: %v", err)
			}
		})
	}
}

func TestArtifactRefRejectsInvalidOrUnavailableEvidenceWithoutApplyingKey(t *testing.T) {
	data := []byte("exact artifact bytes")
	for _, tc := range []struct {
		name   string
		mutate func(*ArtifactOp)
		source ArtifactSource
	}{
		{name: "missing-source"},
		{name: "missing-blob", source: artifactSourceFunc(func(context.Context, string, int64) ([]byte, error) { return nil, blobstore.ErrNotFound })},
		{name: "source-error", source: artifactSourceFunc(func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("read failed") })},
		{name: "wrong-bytes", source: artifactSourceFunc(func(context.Context, string, int64) ([]byte, error) { return bytes.Repeat([]byte("x"), len(data)), nil })},
		{name: "wrong-size", source: artifactSourceFunc(func(context.Context, string, int64) ([]byte, error) { return data[:1], nil })},
		{name: "mixed-empty-data", mutate: func(a *ArtifactOp) { a.Data = []byte{} }},
		{name: "mixed-data", mutate: func(a *ArtifactOp) { a.Data = data }},
		{name: "empty-ref", mutate: func(a *ArtifactOp) { a.Ref = &journal.Ref{} }},
		{name: "invalid-path", mutate: func(a *ArtifactOp) { a.Ref.Path = "../escape" }},
		{name: "invalid-digest", mutate: func(a *ArtifactOp) { a.Ref.Digest = "sha256:no" }},
		{name: "uppercase-digest", mutate: func(a *ArtifactOp) { a.Ref.Digest = strings.ToUpper(a.Ref.Digest) }},
		{name: "negative-size", mutate: func(a *ArtifactOp) { a.Ref.Size = -1 }},
		{name: "excessive-size", mutate: func(a *ArtifactOp) { a.Ref.Size = MaxArtifactRefBytes + 1 }},
		{name: "invalid-integrity", mutate: func(a *ArtifactOp) { a.Ref.Integrity = "unknown" }},
		{name: "conflicting-integrity", mutate: func(a *ArtifactOp) { a.Integrity = apiv1.IntegrityMaintainer }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := tc.source
			if tc.mutate != nil {
				source = artifactSourceFunc(func(context.Context, string, int64) ([]byte, error) {
					t.Error("invalid ref reached the source")
					return data, nil
				})
			}
			w, runsDir := testArtifactWriter(t, WithArtifactSource(source))
			req := artifactRefRequest(t, data)
			if tc.mutate != nil {
				tc.mutate(req.Ops[0].Artifact)
			}
			for i := 0; i < 2; i++ {
				if out, err := w.Emit(context.Background(), req); err == nil || out.Deduplicated != 0 {
					t.Fatalf("invalid evidence acknowledged: %+v, err=%v", out, err)
				}
			}
			if events := readEvents(t, runsDir, req.RunID); len(events) != 1 {
				t.Fatalf("failed evidence wrote artifact/key: %+v", events)
			}
		})
	}
}

func TestArtifactRefMissingEvidenceRemainsRetryableAfterReopen(t *testing.T) {
	ctx := context.Background()
	store := newArtifactBlobDir(t)
	req := artifactRefRequest(t, []byte("published later"))
	w, runsDir := testArtifactWriter(t, WithArtifactSource(store))
	if _, err := w.Emit(ctx, req); err == nil {
		t.Fatal("missing blob acknowledged")
	}
	w.Close()
	w, err := NewWriter(func(string) (string, bool) { return runsDir, true }, WithArtifactSource(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	if _, err := w.Emit(ctx, req); err == nil {
		t.Fatal("missing evidence became a false ACK after reopen")
	}
	if err := store.Put(ctx, req.Ops[0].Artifact.Ref.Digest, []byte("published later")); err != nil {
		t.Fatal(err)
	}
	if out, err := w.Emit(ctx, req); err != nil || out.Applied != 1 {
		t.Fatalf("repaired evidence did not apply: %+v, %v", out, err)
	}
}

func TestArtifactRefUsesAdoptedRunsScrubberBeforePersistingKey(t *testing.T) {
	ctx := context.Background()
	data := []byte("registered-secret")
	registry := journal.NewRegistryScrubber()
	registry.Register(data)
	store := newArtifactBlobDir(t)
	req := artifactRefRequest(t, data)
	if err := store.Put(ctx, req.Ops[0].Artifact.Ref.Digest, data); err != nil {
		t.Fatal(err)
	}
	w, runsDir := testWriter(t, WithArtifactSource(store), WithScrubber(journal.Chain()))
	run, err := journal.Create(runsDir, openHeader(req.RunID).Identity, nil, journal.WithScrubber(registry))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	release, err := w.Adopt(req.RunID, req.Gaggle, run)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := w.Emit(ctx, req); err == nil || !strings.Contains(err.Error(), "after redaction") {
			t.Fatalf("adopted scrub mismatch was acknowledged: %v", err)
		}
	}
	if events := readEvents(t, runsDir, req.RunID); len(events) != 1 {
		t.Fatalf("mismatch persisted an artifact/key: %+v", events)
	}
	if _, err := os.Stat(filepath.Join(runsDir, req.RunID, req.Ops[0].Artifact.Ref.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unscrubbed artifact reached local disk: %v", err)
	}
	release()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	w, err = NewWriter(func(string) (string, bool) { return runsDir, true }, WithArtifactSource(store), WithScrubber(registry))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	if _, err := w.Emit(ctx, req); err == nil {
		t.Fatal("scrub mismatch became false ACK after reopen")
	}
	sanitized := registry.Scrub(data)
	ref, err := journal.ArtifactRef(sanitized)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, ref.Digest, sanitized); err != nil {
		t.Fatal(err)
	}
	req.Ops[0].Artifact.Ref = &ref
	if out, err := w.Emit(ctx, req); err != nil || out.Applied != 1 {
		t.Fatalf("corrected scrubbed evidence did not apply: %+v, %v", out, err)
	}
	if events := readEvents(t, runsDir, req.RunID); len(events) != 2 {
		t.Fatalf("mismatch left intermediate records: %+v", events)
	}
}

func TestArtifactRefWriteFailureDoesNotBecomeFalseAckAfterReopen(t *testing.T) {
	ctx := context.Background()
	data := []byte("artifact survives repair")
	req := artifactRefRequest(t, data)
	store := newArtifactBlobDir(t)
	if err := store.Put(ctx, req.Ops[0].Artifact.Ref.Digest, data); err != nil {
		t.Fatal(err)
	}
	w, runsDir := testArtifactWriter(t, WithArtifactSource(store))
	// The destination cannot be replaced by a regular artifact file.
	destination := filepath.Join(runsDir, req.RunID, req.Ops[0].Artifact.Ref.Path)
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if out, err := w.Emit(ctx, req); err == nil || out.Deduplicated != 0 {
			t.Fatalf("failed write acknowledged: %+v, %v", out, err)
		}
	}
	w.Close()
	w, err := NewWriter(func(string) (string, bool) { return runsDir, true }, WithArtifactSource(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	if out, err := w.Emit(ctx, req); err == nil || out.Deduplicated != 0 {
		t.Fatalf("failed write became false ACK after reopen: %+v, %v", out, err)
	}
	if events := readEvents(t, runsDir, req.RunID); len(events) != 1 {
		t.Fatalf("failed write persisted artifact/key: %+v", events)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if out, err := w.Emit(ctx, req); err != nil || out.Applied != 1 {
		t.Fatalf("repaired destination did not apply: %+v, %v", out, err)
	}
}
