//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

func createPostgresRun(t *testing.T, b *journalpg.Backend) (journal.RunHandle, journal.OwnerToken) {
	t.Helper()
	h, o, err := b.CreateRun(t.Context(), journal.CreateRunRequest{Identity: journal.RunIdentity{RunID: "run-guard", Gaggle: "g", Workflow: "implementation"}})
	if err != nil {
		t.Fatal(err)
	}
	return h, o
}
func TestJournalSeqContiguousUnderConcurrentEmit(t *testing.T) {
	config := postgresJournalConfig(t)
	b := postgresBackend(t, config)
	h, o := createPostgresRun(t, b)
	var wg sync.WaitGroup

	for worker := range 16 {
		wg.Go(func() {
			for batch := range 4 {
				events := []journal.Event{
					{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"emitKey": fmt.Sprintf("worker-%d-batch-%d-a", worker, batch)}},
					{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"emitKey": fmt.Sprintf("worker-%d-batch-%d-b", worker, batch)}},
				}
				if err := appendPostgresRetry(t.Context(), b, h, o, events); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	records, err := b.ReadEvents(t.Context(), journal.RunRef{ID: "run-guard"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 129 {
		t.Fatalf("count=%d", len(records))
	}
	for i, r := range records {
		if r.Event.Seq != uint64(i+1) {
			t.Fatalf("gap at %d: %d", i, r.Event.Seq)
		}
	}
}
func TestStaleOwnerEmitRejected(t *testing.T) {
	config := postgresJournalConfigRenew(t, false)
	b := postgresBackend(t, config)
	h, o := createPostgresRun(t, b)
	old := config.LiveOwner()
	if err := config.Lease.Release(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	manager, err := lease.New(lease.Config{Pool: config.Pool, InstanceID: config.InstanceID, Name: "controller", Holder: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AppendBatch(t.Context(), h, o, []journal.Event{{Type: journal.EventRunnerAnnotation}}); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale write = %v", err)
	}
	config.Lease = manager
	config.LiveOwner = func() *lease.Handle { return next }
	replacement := postgresBackend(t, config)
	recovered, owner, report, err := replacement.RecoverRun(t.Context(), journal.RunKey{RunID: "run-guard", Gaggle: "g"}, journal.RunWriterOptions{})
	if err != nil || report.LastSeq != 1 {
		t.Fatalf("recover=%+v %v", report, err)
	}
	if _, err := replacement.AppendBatch(t.Context(), recovered, owner, []journal.Event{{Type: journal.EventRunnerAnnotation}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AppendBatch(t.Context(), h, o, []journal.Event{{Type: journal.EventRunnerAnnotation}}); err == nil {
		t.Fatal("old token survived takeover")
	}
}
func TestJournalRollbackAndDedupDoNotAllocateGaps(t *testing.T) {
	config := postgresJournalConfig(t)
	b := postgresBackend(t, config)
	h, o := createPostgresRun(t, b)
	_, err := config.Pool.Exec(t.Context(), `ALTER TABLE journal.run_event ADD CONSTRAINT reject_fixture CHECK (body NOT LIKE '%rollback-fixture%'::bytea)`)
	if err != nil {
		t.Fatal(err)
	}
	events := []journal.Event{{Type: journal.EventRunnerAnnotation}, {Type: journal.EventRunnerAnnotation, Name: "rollback-fixture"}}
	if n, err := b.AppendBatch(t.Context(), h, o, events); err == nil || n != 0 {
		t.Fatalf("failed batch=(%d,%v)", n, err)
	}
	event := journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"emitKey": "same-key"}}
	for range 2 {
		if _, err := b.AppendBatch(t.Context(), h, o, []journal.Event{event}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := b.ReadEvents(t.Context(), journal.RunRef{ID: "run-guard"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Event.Seq != 2 {
		t.Fatalf("rollback/dedup allocated gaps: %+v", records)
	}
}
func TestNoNotifyInEmitTransaction(t *testing.T) {
	config := postgresJournalConfig(t)
	b := postgresBackend(t, config)
	listener, err := pgx.ConnectConfig(t.Context(), config.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close(context.Background()) }()
	if _, err := listener.Exec(t.Context(), "LISTEN goobers_journal"); err != nil {
		t.Fatal(err)
	}
	h, o := createPostgresRun(t, b)
	if _, err := b.AppendBatch(t.Context(), h, o, []journal.Event{{Type: journal.EventRunnerAnnotation}}); err != nil {
		t.Fatal(err)
	}
	quiet, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err = listener.WaitForNotification(quiet)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("emit sent notification directly: %v", err)
	}
	notifier, err := pgx.ConnectConfig(t.Context(), config.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = notifier.Close(context.Background()) }()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- b.RunNotifications(ctx, notifier) }()
	defer func() { stop(); <-done }()
	wait, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	notification, err := listener.WaitForNotification(wait)
	if err != nil {
		t.Fatal(err)
	}
	if notification.PID != notifier.PgConn().PID() || notification.Payload != config.InstanceID {
		t.Fatalf("notification came from emit connection: %+v", notification)
	}
	records, err := b.ReadEvents(t.Context(), journal.RunRef{ID: "run-guard"}, 0, 0)
	if err != nil || len(records) != 2 {
		t.Fatalf("notification preceded commit: %d,%v", len(records), err)
	}
}

// Admission includes pool/row-lock wait in its two-second deadline. Saturation
// may refuse a batch; stable emit keys make an uncertain commit safe to retry.
func appendPostgresRetry(ctx context.Context, b *journalpg.Backend, h journal.RunHandle, o journal.OwnerToken, events []journal.Event) error {
	retry, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		_, err := b.AppendBatch(retry, h, o, events)
		if !errors.Is(err, lease.ErrFencedTransactionTimeout) || retry.Err() != nil {
			return err
		}
	}
}

func TestPostgresTranscriptCleanupAndRecovery(t *testing.T) {
	config := postgresJournalConfig(t)
	b := postgresBackend(t, config)
	h, o := createPostgresRun(t, b)
	capture, err := b.BeginTranscript(t.Context(), h, o, 0, "stage", "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("partial output\n"), Reason: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	records, err := b.ReadEvents(t.Context(), journal.RunRef{ID: "run-guard"}, 1, 0)
	if err != nil || len(records) != 1 || records[0].Event.Ref == nil {
		t.Fatalf("partial record=%v,%v", records, err)
	}
	partial := *records[0].Event.Ref
	if _, err := b.ReadBlob(t.Context(), h, partial); err != nil {
		t.Fatal(err)
	}
	final, err := capture.RecordFinalWithExpectedDigest("text/plain", []byte("finished output"), "final-key", journal.Digest([]byte("finished output")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadBlob(t.Context(), h, partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial remains after final: %v", err)
	}
	if data, err := b.ReadBlob(t.Context(), h, final); err != nil || string(data) != "finished output" {
		t.Fatalf("final=%q,%v", data, err)
	}
	if err := b.Retire(t.Context(), h, o); err != nil {
		t.Fatal(err)
	}
	reopened := postgresBackend(t, config)
	h, o, _, err = reopened.RecoverRun(t.Context(), journal.RunKey{RunID: "run-guard", Gaggle: "g"}, journal.RunWriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ReadBlob(t.Context(), h, final); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Retire(t.Context(), h, o); err != nil {
		t.Fatal(err)
	}
}
