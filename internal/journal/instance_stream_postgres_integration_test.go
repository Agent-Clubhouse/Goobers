//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/journal"
)

func TestInstanceCounterContentionUnderFanout(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	handle, owner, _, err := backend.OpenInstance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	const writers = 35 // 32 starts plus 3 concurrent emitters.
	start := make(chan struct{})
	errs := make(chan error, writers)
	durations := make(chan time.Duration, writers)
	var wg sync.WaitGroup
	for index := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			began := time.Now()
			errs <- backend.InstanceAppend(context.Background(), handle, owner, journal.Event{
				Type: journal.EventRunnerAnnotation,
				Name: fmt.Sprintf("burst-%02d", index),
				Runner: map[string]any{
					"emitKey": fmt.Sprintf("burst-%02d", index),
				},
			})
			durations <- time.Since(began)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(durations)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var observed []time.Duration
	for duration := range durations {
		observed = append(observed, duration)
	}
	sort.Slice(observed, func(i, j int) bool { return observed[i] < observed[j] })
	p99 := observed[min(len(observed)-1, (len(observed)*99)/100)]
	t.Logf("instance counter burst p99 lock+commit latency: %s", p99)
	events, err := backend.InstanceRead(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != writers {
		t.Fatalf("events = %d, want %d", len(events), writers)
	}
	for index, event := range events {
		if event.Seq != uint64(index+1) {
			t.Fatalf("event %d seq = %d", index, event.Seq)
		}
	}
}

// TestPartitionedDedup: a scheduler emit key is idempotent no matter which
// month the append-time clock is in. The retry one month later must NOT commit
// a second event.
func TestPartitionedDedup(t *testing.T) {
	config := postgresJournalConfig(t)
	var clockMu sync.Mutex
	at := databaseMonth(t, config)
	config.InstanceClock = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return at
	}
	backend := postgresBackend(t, config)
	handle, owner, _, err := backend.OpenInstance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	event := journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Runner: map[string]any{"emitKey": "monthly-retry"},
	}
	for range 2 {
		if err := backend.InstanceAppend(t.Context(), handle, owner, event); err != nil {
			t.Fatal(err)
		}
	}
	clockMu.Lock()
	at = at.AddDate(0, 1, 0)
	clockMu.Unlock()
	if err := backend.InstanceAppend(t.Context(), handle, owner, event); err != nil {
		t.Fatal(err)
	}
	assertSingleEmitKey(t, config, backend, "monthly-retry")
}

// TestInstanceEmitKeyRetryAcrossMonthBoundary appends a key at next-month-start
// minus one second and retries it one second later (same replica), then from a
// successor whose clock runs a month ahead.
func TestInstanceEmitKeyRetryAcrossMonthBoundary(t *testing.T) {
	config := postgresJournalConfig(t)
	var clockMu sync.Mutex
	var boundary time.Time
	if err := config.Pool.QueryRow(t.Context(), `
		SELECT date_trunc('month',clock_timestamp())+interval '1 month'`).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	boundary = boundary.UTC()
	at := boundary.Add(-time.Second)
	config.InstanceClock = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return at
	}
	backend := postgresBackend(t, config)
	handle, owner, _, err := backend.OpenInstance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	event := journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Runner: map[string]any{"emitKey": "boundary"},
	}
	if err := backend.InstanceAppend(t.Context(), handle, owner, event); err != nil {
		t.Fatal(err)
	}
	for _, retryAt := range []time.Time{boundary.Add(time.Second), boundary.AddDate(0, 1, 0)} {
		clockMu.Lock()
		at = retryAt
		clockMu.Unlock()
		if err := backend.InstanceAppend(t.Context(), handle, owner, event); err != nil {
			t.Fatal(err)
		}
	}
	assertSingleEmitKey(t, config, backend, "boundary")
	// A distinct key after the duplicates still gets the next contiguous seq.
	if err := backend.InstanceAppend(t.Context(), handle, owner, journal.Event{
		Type: journal.EventRunnerAnnotation, Runner: map[string]any{"emitKey": "next"}}); err != nil {
		t.Fatal(err)
	}
	events, err := backend.InstanceRead(t.Context(), 0, 10)
	if err != nil || len(events) != 2 || events[1].Seq != 2 {
		t.Fatalf("events after duplicates = %+v %v", events, err)
	}
}

// TestInstanceEmitKeyWrittenByPreviousBinaryIsDeduplicated: during the Recreate
// rollout the N-1 controller appends scheduler events (with emit keys) after
// the migration's backfill and knows nothing of instance_emit_key. The trigger
// keeps the key table complete, so the successor's retry of that append dedups.
func TestInstanceEmitKeyWrittenByPreviousBinaryIsDeduplicated(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	handle, owner, _, err := backend.OpenInstance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// What #7853's InstanceAppend does: bump last_seq, insert the event, no key table.
	if _, err := config.Pool.Exec(t.Context(), `UPDATE journal.instance_seq SET last_seq=1 WHERE instance_id=$1 AND stream='scheduler'`, config.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Pool.Exec(t.Context(), `INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body,run_id)
		VALUES ($1,'scheduler',1,clock_timestamp(),'runner.annotation','n1-key',1,'{}','')`, config.InstanceID); err != nil {
		t.Fatal(err)
	}
	event := journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"emitKey": "n1-key"}}
	if err := backend.InstanceAppend(t.Context(), handle, owner, event); err != nil {
		t.Fatal(err)
	}
	var rows int
	var last int64
	if err := config.Pool.QueryRow(t.Context(), `SELECT count(*) FROM journal.instance_event WHERE instance_id=$1 AND emit_key='n1-key'`, config.InstanceID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := config.Pool.QueryRow(t.Context(), `SELECT last_seq FROM journal.instance_seq WHERE instance_id=$1 AND stream='scheduler'`, config.InstanceID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || last != 1 {
		t.Fatalf("N-1 emit key retried: rows=%d last_seq=%d, want 1 and 1", rows, last)
	}
}

func assertSingleEmitKey(t *testing.T, config journalpg.Config, backend *journalpg.Backend, key string) {
	t.Helper()
	events, err := backend.InstanceRead(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("events = %+v, want one event with seq 1", events)
	}
	var rows int
	if err := config.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM journal.instance_event
		WHERE instance_id=$1 AND emit_key=$2`, config.InstanceID, key).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	var last int64
	if err := config.Pool.QueryRow(t.Context(), `
		SELECT last_seq FROM journal.instance_seq WHERE instance_id=$1 AND stream='scheduler'`,
		config.InstanceID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || last != 1 {
		t.Fatalf("rows with emit key = %d, last_seq = %d, want 1 and 1", rows, last)
	}
}

func TestHealthStreamAndConsumerCursor(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	handle, owner, _, err := backend.OpenHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	at := databaseMonth(t, config)
	event := apiv1.GaggleHealthEvent{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		OccurredAt:    at, Type: apiv1.GaggleHealthEvaluated, Gaggle: "alpha",
	}
	if err := backend.HealthAppend(t.Context(), handle, owner, event); err != nil {
		t.Fatal(err)
	}
	events, err := backend.HealthRead(t.Context(), 0, 10)
	if err != nil || len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("health events = %+v %v", events, err)
	}
	cursor, err := backend.LoadStreamCursor(
		t.Context(), "health-rollup", journal.StreamHealth)
	if err != nil || cursor.Seq != 0 {
		t.Fatalf("initial cursor = %+v %v", cursor, err)
	}
	if err := backend.AdvanceStreamCursor(
		t.Context(), "health-rollup", cursor, 1); err != nil {
		t.Fatal(err)
	}
	if err := backend.AdvanceStreamCursor(
		t.Context(), "health-rollup", cursor, 1); !errors.Is(err, journal.ErrStreamCursorConflict) {
		t.Fatalf("stale cursor CAS = %v", err)
	}
	current, err := backend.LoadStreamCursor(
		t.Context(), "health-rollup", journal.StreamHealth)
	if err != nil || current != (journal.StreamCursor{Stream: journal.StreamHealth, Seq: 1}) {
		t.Fatalf("current cursor = %+v %v", current, err)
	}
	if err := backend.AdvanceStreamCursor(
		t.Context(), "health-rollup", current, 0); err == nil {
		t.Fatal("consumer cursor moved backward")
	}
}

func databaseMonth(t *testing.T, config journalpg.Config) time.Time {
	t.Helper()
	var at time.Time
	if err := config.Pool.QueryRow(t.Context(), `
		SELECT date_trunc('month',clock_timestamp())+interval '1 day'`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.UTC()
}
