package repair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

type unreadableRunBackend struct {
	journal.ReaderBackend
	runID    string
	attempts int
}

func (b *unreadableRunBackend) OpenRun(ctx context.Context, ref journal.RunRef) (journal.RunReader, error) {
	if ref.ID == b.runID {
		b.attempts++
		return nil, errors.New("corrupt run.yaml")
	}
	return b.ReaderBackend.OpenRun(ctx, ref)
}

// A persistently unreadable projected run must not block discovery, even with
// one-entry batches and a sweeper restart. Keep its projection for diagnosis
// and retry it on later reverse cycles.
func TestPersistentlyUnreadableRowDoesNotWedgeBackendRepair(t *testing.T) {
	for _, batchSize := range []int{1, 2, 10} {
		t.Run(fmt.Sprintf("batch-%d", batchSize), func(t *testing.T) {
			ctx := t.Context()
			store := openStore(t)
			root := t.TempDir()
			writeRun(t, root, "run-a-poison")
			file := journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{root}})
			reader, err := file.OpenRun(ctx, journal.RunRef{ID: "run-a-poison"})
			if err != nil {
				t.Fatal(err)
			}
			identity, err := reader.Identity()
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			projection, err := readmodel.ProjectRunFromJournal(reader, identity, events)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertRun(ctx, projection); err != nil {
				t.Fatal(err)
			}
			before, found, err := store.GetRun(ctx, "run-a-poison")
			if err != nil || !found {
				t.Fatalf("seed projection: found=%v err=%v", found, err)
			}
			writeRun(t, root, "run-b-new")
			backend := &unreadableRunBackend{ReaderBackend: file, runID: "run-a-poison"}
			var logs bytes.Buffer
			now := time.Now().Add(time.Hour)
			options := Options{Journal: backend, BatchSize: batchSize,
				Logger: slog.New(slog.NewTextHandler(&logs, nil)),
				Now:    func() time.Time { return now }}
			failures := 0
			for step := 0; step < 5; step++ {
				// Restart between steps so progress depends on the durable cursor.
				sweeper := New(store, store, nil, options)
				if err := sweeper.Step(ctx); err != nil {
					t.Fatalf("step %d: %v", step, err)
				}
				stats := sweeper.Stats()
				failures += stats.Failures
				if stats.EntriesExamined > batchSize {
					t.Fatalf("step %d exceeded batch budget: %+v", step, stats)
				}
				if step == 0 && batchSize <= 2 {
					cursor, err := store.SweepCursor(ctx)
					if err != nil || cursor.ReverseAfterRunID != backend.runID {
						t.Fatalf("failed lookup did not advance reverse cursor: %+v err=%v", cursor, err)
					}
				}
			}
			if _, found, err := store.GetRun(ctx, "run-b-new"); err != nil || !found {
				t.Fatalf("new run not discovered after five steps: found=%v err=%v", found, err)
			}
			after, found, err := store.GetRun(ctx, backend.runID)
			if err != nil || !found || !reflect.DeepEqual(before, after) {
				t.Fatalf("unreadable run's projection changed: found=%v err=%v before=%+v after=%+v", found, err, before, after)
			}
			if backend.attempts < 2 || failures != backend.attempts {
				t.Fatalf("lookup retries not counted exactly once: attempts=%d failures=%d", backend.attempts, failures)
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), backend.runID) || !strings.Contains(logs.String(), "corrupt run.yaml") {
				t.Fatalf("missing diagnostic: %s", logs.String())
			}
		})
	}
}
