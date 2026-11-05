//go:build cloudintegration

package livejournal

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/journal"
)

func pgReadWriter(t *testing.T, opts ...Option) (*Writer, *journalpg.Backend, string) {
	t.Helper()
	admin := pgtest.Pool(t, "livejournal")
	config := admin.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var role string
	if err := pool.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "goobers_controller" {
		t.Fatalf("current_user=%q", role)
	}
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: pgtest.InstanceID, Name: lease.ControllerName, Holder: "livejournal", TTL: 2 * time.Minute, SelfFenceAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	handle := pgtest.Acquire(t, manager)
	t.Cleanup(func() { _ = manager.Release(context.Background(), handle) })
	backend, err := journalpg.New(journalpg.Config{Pool: pool, InstanceID: pgtest.InstanceID, Lease: manager, LiveOwner: func() *lease.Handle { return handle }})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	opts = append(opts, WithBackend(backend))
	writer, err := NewWriter(func(string) (string, bool) { return root, true }, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	t.Cleanup(func() {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Error(err)
		} else if len(entries) != 0 {
			t.Errorf("PG journal wrote local files: %v", entries)
		}
	})
	return writer, backend, root
}

func TestPostgresChildAppendReconciliationReadsCommittedEvents(t *testing.T) {
	w, _, _ := pgReadWriter(t)
	batch := openBatch(strings.Repeat("a", 32), time.Now())
	batch.Ops[1].ChildStageStart = &ChildStageStart{}
	first, err := w.Emit(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	run, err := w.acquire(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	run.mu.Lock()
	// Simulate discarded caches after an uncertain child append; reconciliation
	// must re-derive authority from committed PG rows, not the local filesystem.
	run.keys = make(map[string]uint64)
	run.childStarts = nil
	run.childAppendUncertain = true
	err = w.reconcileChildAppend(run)
	run.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	retry, err := w.Emit(t.Context(), batch)
	if err != nil || len(retry.ChildOrigins) != 1 || retry.ChildOrigins[0] != first.ChildOrigins[0] || retry.Applied != 0 {
		t.Fatalf("reconciled retry=%+v, %v; first=%+v", retry, err, first)
	}
}

func TestPostgresTranscriptAdoptionReadsCommittedHistoryAndBlob(t *testing.T) {
	w, backend, _ := pgReadWriter(t)
	runID := strings.Repeat("b", 32)
	batch := openBatch(runID, time.Now())
	if _, err := w.Emit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	capture := strings.Repeat("c", 32)
	send := func(action, key string, request TranscriptCheckpointOp) {
		request.Capture = capture
		request.Action = action
		request.Stage = "build"
		request.Name = "fixture.transcript"
		_, err := w.Emit(t.Context(), EmitRequest{RunID: runID, Gaggle: "web", Ops: []Op{{Kind: OpTranscriptCheckpoint, Key: capture + "/" + key, Time: time.Now(), Checkpoint: &request}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	send("open", "open", TranscriptCheckpointOp{})
	data := []byte("durable transcript\n")
	send("append", "checkpoint/0", TranscriptCheckpointOp{Stream: "process-output/1", Data: data, Reason: "checkpoint"})
	ref, err := journal.SpanRef(data)
	if err != nil {
		t.Fatal(err)
	}
	send("final", "final", TranscriptCheckpointOp{Data: data, FinalRef: &ref, InlineFinal: true})
	w.Close()
	op := Op{Kind: OpSpan, Key: "workflow-adopt", Time: time.Now(), Span: &SpanOp{Stage: "build", Name: "fixture.transcript", Ref: ref}}
	for range 2 {
		if _, err := w.Emit(t.Context(), EmitRequest{RunID: runID, Gaggle: "web", Ops: []Op{op}}); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := backend.OpenRunReader(t.Context(), journal.RunKey{Gaggle: "web", RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	complete, adopted := 0, 0
	var completedCapture, adoptedCapture string
	for _, event := range events {
		if value, ok := event.Runner["transcriptCaptureComplete"].(string); ok {
			completedCapture = value
			if !validRemoteCaptureID(value) {
				t.Errorf("invalid durable capture ID %q", value)
			}
			complete++
		}
		if value, ok := event.Runner["transcriptCaptureAdopted"].(string); ok {
			adoptedCapture = value
			adopted++
		}
	}
	if complete != 1 || adopted != 1 || completedCapture != adoptedCapture {
		t.Fatalf("completion=%d adoption=%d events=%+v", complete, adopted, events)
	}
}

func TestPostgresArtifactPublicationReadsCommittedScrubbedBlob(t *testing.T) {
	store := newBlobDir(t)
	w, backend, _ := pgReadWriter(t, WithSpanSource(store), WithScrubber(journal.NewPatternScrubber()))
	runID := strings.Repeat("d", 32)
	now := time.Now()
	if _, err := w.Emit(t.Context(), openBatch(runID, now)); err != nil {
		t.Fatal(err)
	}
	raw := []byte("value ghp_" + strings.Repeat("A", 80))
	op := verdictOp(runID, now, raw)
	op.Ops[0].Artifact.Integrity = apiv1.IntegrityDerived
	for range 2 {
		if _, err := w.Emit(t.Context(), op); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := backend.OpenRunReader(t.Context(), journal.RunKey{Gaggle: "web", RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Ref == nil {
		t.Fatalf("artifact events=%+v", events)
	}
	committed, err := reader.ArtifactBytes(*events[2].Ref)
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.Get(t.Context(), events[2].Ref.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(published) || strings.Contains(string(published), "ghp_") || !strings.Contains(string(published), journal.Redacted) {
		t.Fatalf("published=%q committed=%q", published, committed)
	}
}
