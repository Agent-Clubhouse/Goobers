//go:build cloudintegration

package journal_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// TestPostgresRunEventRecordsDatabaseClock: every stored run event carries the
// database's commit clock beside the writer's own Time, however far the writer
// clock is skewed, so stall accounting can ignore writer clocks.
func TestPostgresRunEventRecordsDatabaseClock(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	owner := postgresOwnerPool(t, config)
	ctx := t.Context()
	var before time.Time
	if err := owner.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for name, skew := range map[string]time.Duration{"ahead": 6 * time.Hour, "behind": -6 * time.Hour} {
		runID := "run-skewed-" + name
		h, o, err := backend.CreateRun(ctx, journal.CreateRunRequest{
			Identity: journal.RunIdentity{RunID: runID, Gaggle: "g", Workflow: "implementation"},
			Options:  journal.RunWriterOptions{Clock: func() time.Time { return time.Now().Add(skew) }},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.AppendBatch(ctx, h, o, []journal.Event{{Type: journal.EventRunnerAnnotation, Name: "skewed"}}); err != nil {
			t.Fatal(err)
		}
		var after time.Time
		if err := owner.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		records, err := backend.ReadEvents(ctx, journal.RunRef{ID: runID, ReadOnly: true}, 0, 0)
		if err != nil || len(records) == 0 {
			t.Fatalf("%s: read = %d records, %v", name, len(records), err)
		}
		for _, record := range records {
			if record.RecordedAt.Before(before) || record.RecordedAt.After(after) {
				t.Fatalf("%s: seq %d recorded at %s, outside the database window [%s, %s]",
					name, record.Event.Seq, record.RecordedAt, before, after)
			}
			if gap := record.Event.Time.Sub(record.RecordedAt); (skew > 0 && gap < time.Hour) || (skew < 0 && gap > -time.Hour) {
				t.Fatalf("%s: premise: writer time %s is not skewed from %s", name, record.Event.Time, record.RecordedAt)
			}
		}
	}
}

// TestPostgresPreviousWriterInsertIsStampedByDefault: 0025 is an expand, so
// the previous binary's run_event INSERT, which never names recorded_at, must
// keep working as the controller and be stamped by the column default. Rows
// stored before 0025 (recorded_at NULL) read back with a zero RecordedAt.
func TestPostgresPreviousWriterInsertIsStampedByDefault(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	owner := postgresOwnerPool(t, config)
	ctx := t.Context()
	h, o := createPostgresRun(t, backend)
	if _, err := backend.AppendBatch(ctx, h, o, []journal.Event{{Type: journal.EventRunnerAnnotation, Name: "current"}}); err != nil {
		t.Fatal(err)
	}
	var token string
	var epoch int64
	if err := owner.QueryRow(ctx, `SELECT owner_token, owner_epoch FROM journal.run_seq WHERE instance_id=$1 AND run_id='run-guard'`,
		config.InstanceID).Scan(&token, &epoch); err != nil {
		t.Fatal(err)
	}
	body := func(seq uint64) []byte {
		t.Helper()
		raw, err := json.Marshal(journal.Event{Schema: journal.EventSchema, Seq: seq, Type: journal.EventRunnerAnnotation, Name: "previous", Time: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	// The previous binary's appendClean INSERT, verbatim, on the controller pool.
	tag, err := config.Pool.Exec(ctx, `INSERT INTO journal.run_event(instance_id,run_id,seq,branch,type,stage,attempt,emit_key,at,body,operator_key,notification_key,notification_sink,notification_attempt,agent_id,agent_run,agent_stage,agent_attempt,at_submicro)
 SELECT instance_id,run_id,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21 FROM journal.run_seq WHERE instance_id=$1 AND run_id=$2 AND owner_token=$3 AND owner_epoch=$4`,
		config.InstanceID, "run-guard", token, epoch, int64(1000), 0, string(journal.EventRunnerAnnotation), "", 0, nil,
		time.Now().UTC().Truncate(time.Microsecond), body(1000), "", "", "", 0, "", "", "", 0, 0)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("previous writer insert = %v, %v", tag, err)
	}
	// A row stored before 0025: no database-clock stamp.
	if _, err := owner.Exec(ctx, `
		INSERT INTO journal.run_event (instance_id,run_id,seq,type,at,body,recorded_at)
		VALUES ($1,'run-guard',1001,'runner.annotation',clock_timestamp(),$2,NULL)`,
		config.InstanceID, body(1001)); err != nil {
		t.Fatal(err)
	}
	records, err := backend.ReadEvents(ctx, journal.RunRef{ID: "run-guard", ReadOnly: true}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	stamped := map[uint64]bool{}
	for _, record := range records {
		stamped[record.Event.Seq] = !record.RecordedAt.IsZero()
	}
	if !stamped[1000] {
		t.Fatalf("previous writer's row was not stamped by the default: %v", stamped)
	}
	if stamped[1001] {
		t.Fatalf("pre-0025 row read back with a RecordedAt: %v", stamped)
	}
	for seq, ok := range stamped {
		if seq < 1000 && !ok {
			t.Fatalf("current writer's seq %d not stamped: %v", seq, stamped)
		}
	}
}
