//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/journal"
)

func TestPostgresLargeHistoryFinishesAndRecovers(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	handle, owner := createPostgresRun(t, backend)
	seedPostgresHistory(t, config, owner, 10000)
	if err := backend.Checkpoint(t.Context(), handle, owner); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{Type: journal.EventRunFinished, Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	t.Logf("finish after 10k events: %s (write fence remains 2s)", time.Since(started))
	status, err := backend.RunStatus(t.Context(), handle)
	if err != nil || status.Seq != 10002 || status.Phase != journal.PhaseCompleted {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if err := backend.Retire(t.Context(), handle, owner); err != nil {
		t.Fatal(err)
	}
	_, _, report, err := backend.RecoverRun(t.Context(), journal.RunKey{RunID: "run-guard", Gaggle: "g"}, journal.RunWriterOptions{})
	if err != nil || report.LastSeq != 10002 || len(report.Events) != 10002 {
		t.Fatalf("recover count=%d last=%d err=%v", len(report.Events), report.LastSeq, err)
	}
}

// Seed a realistic 10 MiB immutable history in bounded fenced batches. Bulk SQL
// keeps fixture construction cheap; actual finish/checkpoint/recovery use only
// the production APIs and have no awareness that the fixture was bulk-loaded.
func seedPostgresHistory(t *testing.T, config journalpg.Config, owner journal.OwnerToken, count int) {
	t.Helper()
	raw, err := journal.MarshalEvent(journal.Event{Schema: journal.EventSchema, Seq: 1, Time: time.Now(), Type: journal.EventRunnerAnnotation, Name: "history", Runner: map[string]any{"note": strings.Repeat("x", 1024)}})
	if err != nil {
		t.Fatal(err)
	}
	for start := 2; start < count+2; start += 500 {
		end := min(start+499, count+1)
		live := config.LiveOwner()
		err := config.Lease.WithFence(t.Context(), live, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `INSERT INTO journal.run_event(instance_id,run_id,seq,type,at,body)
 SELECT s.instance_id,s.run_id,n,'runner.annotation',clock_timestamp(),convert_to(jsonb_set($5::jsonb,'{seq}',to_jsonb(n))::text,'UTF8')
 FROM journal.run_seq s CROSS JOIN generate_series($6::bigint,$7::bigint) n
 WHERE s.instance_id=$1 AND s.run_id=$2 AND s.owner_token=$3 AND s.owner_epoch=$4`, config.InstanceID, "run-guard", owner, live.Epoch, string(raw), start, end)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != int64(end-start+1) {
				return fmt.Errorf("seed wrote %d rows", tag.RowsAffected())
			}
			_, err = tx.Exec(ctx, `UPDATE journal.run_seq SET last_seq=$5 WHERE instance_id=$1 AND run_id=$2 AND owner_token=$3 AND owner_epoch=$4`, config.InstanceID, "run-guard", owner, live.Epoch, end)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresPredicateOutsideFenceRechecksConcurrentAppend(t *testing.T) {
	backend := postgresBackend(t, postgresJournalConfig(t))
	handle, owner := createPostgresRun(t, backend)
	entered := make(chan struct{})
	resume := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(resume) })
	var first sync.Once
	result := make(chan error, 1)
	go func() {
		appended, err := backend.AppendIfAbsent(t.Context(), handle, owner, journal.Event{Type: journal.EventRunnerAnnotation, Name: "marker"}, func(event journal.Event) bool {
			first.Do(func() { close(entered); <-resume })
			return event.Name == "marker"
		})
		if err == nil && appended {
			err = fmt.Errorf("predicate missed concurrent marker")
		}
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("predicate did not start")
	}
	// This append must finish while the predicate is blocked. The old fenced
	// predicate held the counter lock until the two-second timeout instead.
	if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{Type: journal.EventRunnerAnnotation, Name: "marker"}}); err != nil {
		t.Fatal(err)
	}
	unblock.Do(func() { close(resume) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("predicate tail fold did not finish")
	}
	records, err := backend.ReadEvents(t.Context(), journal.RunRef{ID: "run-guard"}, 0, 0)
	if err != nil || len(records) != 2 {
		t.Fatalf("count=%d err=%v", len(records), err)
	}
}

func TestPostgresProgressWindowDedupAndAtomicBatch(t *testing.T) {
	backend := postgresBackend(t, postgresJournalConfig(t))
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h, o, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: journal.RunIdentity{RunID: "run-progress", Gaggle: "g", Workflow: "implementation"}, Options: journal.RunWriterOptions{Clock: func() time.Time { return now }}})
	if err != nil {
		t.Fatal(err)
	}
	event := func(key string) journal.Event {
		return journal.Event{Type: journal.EventAgentProgress, Runner: map[string]any{"emitKey": key}, Progress: &journal.AgentProgress{Schema: "goobers.dev/journal/agent-progress/v1", AgentID: "worker", RunID: "run-progress", Stage: "work", Attempt: 1, Kind: journal.AgentProgressSummary, Source: journal.AgentProgressSourceModel, Summary: "working"}}
	}
	batch := make([]journal.Event, journal.AgentProgressRateLimitMax-1)
	for i := range batch {
		batch[i] = event(fmt.Sprintf("p-%d", i))
	}
	if _, err := backend.AppendBatch(t.Context(), h, o, batch); err != nil {
		t.Fatal(err)
	}
	if n, err := backend.AppendBatch(t.Context(), h, o, []journal.Event{event("last"), event("over")}); !errors.Is(err, journal.ErrAgentProgressRateLimited) || n != 0 {
		t.Fatalf("overflow batch=%d,%v", n, err)
	}
	for range 2 {
		if _, err := backend.AppendBatch(t.Context(), h, o, []journal.Event{event("last")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backend.AppendBatch(t.Context(), h, o, []journal.Event{event("over")}); !errors.Is(err, journal.ErrAgentProgressRateLimited) {
		t.Fatalf("rate limit=%v", err)
	}
	now = now.Add(journal.AgentProgressRateLimitWindow)
	if _, err := backend.AppendBatch(t.Context(), h, o, []journal.Event{event("boundary")}); !errors.Is(err, journal.ErrAgentProgressRateLimited) {
		t.Fatalf("inclusive window boundary = %v", err)
	}
	now = now.Add(time.Nanosecond)
	if _, err := backend.AppendBatch(t.Context(), h, o, []journal.Event{event("next-window")}); err != nil {
		t.Fatal(err)
	}
	status, err := backend.RunStatus(t.Context(), h)
	if err != nil || status.Seq != uint64(journal.AgentProgressRateLimitMax+2) {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
