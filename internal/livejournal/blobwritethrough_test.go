package livejournal

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

// flakyStore is a blob store whose Put fails while failPuts > 0.
type flakyStore struct {
	*blobstore.Dir
	failPuts int
}

func (f *flakyStore) Put(ctx context.Context, digest string, data []byte) error {
	if f.failPuts > 0 {
		f.failPuts--
		return errors.New("blob plane unavailable")
	}
	return f.Dir.Put(ctx, digest, data)
}

func newBlobDir(t *testing.T) *blobstore.Dir {
	t.Helper()
	store, err := blobstore.NewDir(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func verdictOp(runID string, at time.Time, data []byte) EmitRequest {
	return EmitRequest{RunID: runID, Gaggle: "web", Ops: []Op{
		{Kind: OpArtifact, Key: runID + "|0||0|1", Time: at, Artifact: &ArtifactOp{
			Name: "verdict/review-1.json", Data: data, Integrity: apiv1.IntegrityDerived,
		}},
	}}
}

func recordedArtifacts(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	var out []journal.Event
	for _, ev := range readEvents(t, runsDir, runID) {
		if ev.Type == journal.EventArtifactRecorded {
			out = append(out, ev)
		}
	}
	return out
}

// #5550: a verdict the engine authors reaches the daemon only as an emitted
// artifact op. The engine points the next dispatch at the digest of the bytes
// it marshalled, and a pod resolves that pointer ONLY through the blob plane,
// so the committed artifact must be there under exactly that digest.
func TestEmitArtifactWritesThroughToBlobPlaneUnderPointerDigest(t *testing.T) {
	store := newBlobDir(t)
	w, runsDir := testWriter(t, WithSpanSource(store))
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-wt", started)); err != nil {
		t.Fatal(err)
	}
	verdict := []byte(`{"decision":"needs-changes","findings":[{"id":1,"summary":"missing test"}]}`)
	pointer, err := journal.ArtifactRef(verdict) // what engine.gateEvaluated injects
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Emit(context.Background(), verdictOp("run-wt", started.Add(time.Second), verdict)); err != nil {
		t.Fatal(err)
	}

	recorded := recordedArtifacts(t, runsDir, "run-wt")
	if len(recorded) != 1 || recorded[0].Ref == nil {
		t.Fatalf("recorded artifacts = %+v, want one", recorded)
	}
	if recorded[0].Ref.Digest != pointer.Digest {
		t.Fatalf("journal committed %s, engine pointer is %s: scrubbing diverged", recorded[0].Ref.Digest, pointer.Digest)
	}
	got, err := store.Get(context.Background(), pointer.Digest)
	if err != nil {
		t.Fatalf("pointer digest not in the blob plane: %v", err)
	}
	if journal.Digest(got) != pointer.Digest || !bytes.Equal(got, verdict) {
		t.Fatalf("blob plane holds %q (digest %s), want the verdict under %s", got, journal.Digest(got), pointer.Digest)
	}
}

// The blob plane must hold what the journal COMMITTED — the scrubbed bytes,
// under their own digest — never the raw op payload.
func TestEmitArtifactWriteThroughPublishesScrubbedBytes(t *testing.T) {
	store := newBlobDir(t)
	w, runsDir := testWriter(t, WithSpanSource(store))
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-scrub", started)); err != nil {
		t.Fatal(err)
	}
	token := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789AB"
	raw := []byte(`{"decision":"fail","detail":"leaked ` + token + `"}`)
	if _, err := w.Emit(context.Background(), verdictOp("run-scrub", started.Add(time.Second), raw)); err != nil {
		t.Fatal(err)
	}
	recorded := recordedArtifacts(t, runsDir, "run-scrub")
	if len(recorded) != 1 || recorded[0].Ref == nil {
		t.Fatalf("recorded artifacts = %+v, want one", recorded)
	}
	committed := recorded[0].Ref.Digest
	if committed == journal.Digest(raw) {
		t.Fatal("precondition: the scrubber did not change the payload, so this test proves nothing")
	}
	got, err := store.Get(context.Background(), committed)
	if err != nil {
		t.Fatalf("committed digest not in the blob plane: %v", err)
	}
	if journal.Digest(got) != committed || bytes.Contains(got, []byte(token)) {
		t.Fatalf("blob plane holds unscrubbed or mismatched bytes: %q", got)
	}
	if ok, err := store.Has(context.Background(), journal.Digest(raw)); err != nil || ok {
		t.Fatalf("raw payload published to the blob plane (has=%v err=%v)", ok, err)
	}
}

// A failed write-through fails the emit, but the journal already committed the
// artifact, so the retry is deduplicated. The retry must still publish — and
// must not append a second artifact.recorded.
func TestEmitArtifactWriteThroughRetriesOnDeduplicatedRedelivery(t *testing.T) {
	store := &flakyStore{Dir: newBlobDir(t), failPuts: 1}
	w, runsDir := testWriter(t, WithSpanSource(store))
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-flaky", started)); err != nil {
		t.Fatal(err)
	}
	verdict := []byte(`{"decision":"pass"}`)
	batch := verdictOp("run-flaky", started.Add(time.Second), verdict)
	if _, err := w.Emit(context.Background(), batch); err == nil {
		t.Fatal("emit succeeded although the blob plane refused the write-through")
	}
	resp, err := w.Emit(context.Background(), batch)
	if err != nil {
		t.Fatalf("redelivered emit: %v", err)
	}
	if resp.Applied != 0 || resp.Deduplicated != 1 {
		t.Fatalf("redelivered resp = %+v, want deduplicated", resp)
	}
	if n := len(recordedArtifacts(t, runsDir, "run-flaky")); n != 1 {
		t.Fatalf("journal holds %d artifact.recorded events, want 1", n)
	}
	if _, err := store.Get(context.Background(), journal.Digest(verdict)); err != nil {
		t.Fatalf("redelivery did not publish the verdict: %v", err)
	}
}

// The same retry must heal across a writer restart, where the dedup state is
// re-derived from the journal rather than remembered.
func TestEmitArtifactWriteThroughRetriesAfterRestart(t *testing.T) {
	store := &flakyStore{Dir: newBlobDir(t), failPuts: 1}
	w, runsDir := testWriter(t, WithSpanSource(store))
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-restart-wt", started)); err != nil {
		t.Fatal(err)
	}
	verdict := []byte(`{"decision":"pass"}`)
	batch := verdictOp("run-restart-wt", started.Add(time.Second), verdict)
	if _, err := w.Emit(context.Background(), batch); err == nil {
		t.Fatal("emit succeeded although the blob plane refused the write-through")
	}
	w.Close()

	restarted, err := NewWriter(func(string) (string, bool) { return runsDir, true }, WithSpanSource(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if _, err := restarted.Emit(context.Background(), batch); err != nil {
		t.Fatalf("redelivered emit after restart: %v", err)
	}
	if _, err := store.Get(context.Background(), journal.Digest(verdict)); err != nil {
		t.Fatalf("redelivery after restart did not publish the verdict: %v", err)
	}
}

// A span source that cannot Put (every read-only assembly) leaves the
// write-through off: the artifact op commits exactly as before.
func TestEmitArtifactWithoutSinkIsUnchanged(t *testing.T) {
	w, runsDir := testWriter(t, WithSpanSource(&fakeSpans{}))
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-nosink", started)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Emit(context.Background(), verdictOp("run-nosink", started.Add(time.Second), []byte(`{"decision":"pass"}`))); err != nil {
		t.Fatal(err)
	}
	if n := len(recordedArtifacts(t, runsDir, "run-nosink")); n != 1 {
		t.Fatalf("journal holds %d artifact.recorded events, want 1", n)
	}
}
