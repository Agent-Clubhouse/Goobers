package triggerqueue

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func restartRequest(key string) HumanRestartAcceptance {
	return HumanRestartAcceptance{Key: key, Actor: "human", Gaggle: "web", SourceRun: "source", Stage: "work", Epoch: key, TerminalSequence: 7, Payload: []byte("{}"), Plan: []byte("pinned " + key)}
}
func restartWithControl(t *testing.T, q *Store, key string) StartControl {
	t.Helper()
	r, _, err := q.AcceptHumanRestart(t.Context(), restartRequest(key), childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	scope := controlScope(r, "human-restart")
	scope.ReservedRunID = key
	c, err := q.PinStartControl(t.Context(), r.ID, scope)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestExplicitRestartAfterProvenUnattemptedCancellationPreservesOldReceipt(t *testing.T) {
	for _, disposition := range []string{"cancelled", "expired"} {
		t.Run(disposition, func(t *testing.T) {
			q := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
			first := restartWithControl(t, q, "first")
			if _, _, err := q.AcceptHumanRestart(t.Context(), restartRequest("second"), childTestTime); !errors.Is(err, ErrConflict) {
				t.Fatal("simultaneous epoch", err)
			}
			if disposition == "cancelled" {
				if _, _, err := q.RequestStartCancellation(t.Context(), "web", first.Record.ID, controlCommand(), childTestTime); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, _, err := q.ExpireStartControl(t.Context(), "web", first.Record.ID, first.Scope.Deadline); err != nil {
					t.Fatal(err)
				}
			}
			second, dup, err := q.AcceptHumanRestart(t.Context(), restartRequest("second"), first.Scope.Deadline.Add(time.Second))
			if err != nil || dup || second.ID == first.Record.ID || second.State != Accepted {
				t.Fatal(second, dup, err)
			}
			replay, dup, err := q.AcceptHumanRestart(t.Context(), restartRequest("first"), first.Scope.Deadline.Add(time.Minute))
			if err != nil || !dup || replay.ID != first.Record.ID || replay.State != Rejected || replay.RunID != "" {
				t.Fatal("revived old command", replay, dup, err)
			}
			prior, err := q.HumanRestartPlan(t.Context(), first.Record.ID)
			if err != nil || !bytes.Equal(prior, restartRequest("first").Plan) {
				t.Fatal("old context changed", err)
			}
			if _, _, err = q.AcceptHumanRestart(t.Context(), restartRequest("third"), childTestTime); !errors.Is(err, ErrConflict) {
				t.Fatal("second epoch lost occurrence slot", err)
			}
		})
	}
}
func TestAttemptedRestartCancellationNeverReleasesOccurrence(t *testing.T) {
	q := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := restartWithControl(t, q, "attempted")
	if err := q.BeginDispatchAt(t.Context(), c.Record.ID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.RequestStartCancellation(t.Context(), "web", c.Record.ID, controlCommand(), childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CompleteStartCancellation(t.Context(), "web", c.Record.ID, "cancel-1", "confirmed", childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.AcceptHumanRestart(t.Context(), restartRequest("new"), childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal("uncertain epoch resent", err)
	}
}
func TestRestartRetryMigrationPreservesExistingEpochAndOccurrence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(migrations, humanRestartRetrySchema)
	if index < 1 {
		t.Fatal("migration missing")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:index]); err != nil {
		t.Fatal(err)
	}
	req := restartRequest("old")
	if _, err = db.Exec(`INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES('old-id',?,?,?,'accepted',?)`, req.Key, req.Actor, req.Payload, childTestTime.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO human_restart_plans(acceptance_id,gaggle,source_run,terminal_seq,stage,epoch,plan) VALUES('old-id',?,?,?,?,?,?)`, req.Gaggle, req.SourceRun, req.TerminalSequence, req.Stage, req.Epoch, req.Plan); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	q := openTestStore(t, path)
	if _, _, err = q.AcceptHumanRestart(t.Context(), restartRequest("new"), childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal("migration released live slot", err)
	}
	got, dup, err := q.AcceptHumanRestart(t.Context(), req, childTestTime)
	if err != nil || !dup || got.ID != "old-id" {
		t.Fatal(got, dup, err)
	}
}

func TestHumanRestartCompactionPreservesQuotaCustodyAndRequiresRetirement(t *testing.T) {
	q := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	req := restartRequest("compact")
	req.Plan = bytes.Repeat([]byte("x"), 2<<20)
	r, _, err := q.AcceptHumanRestart(t.Context(), req, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	scope := controlScope(r, "human-restart")
	scope.ReservedRunID = req.Epoch
	if _, err = q.PinStartControl(t.Context(), r.ID, scope); err != nil {
		t.Fatal(err)
	}
	if _, _, err = q.RequestStartCancellation(t.Context(), "web", r.ID, controlCommand(), childTestTime); err != nil {
		t.Fatal(err)
	}
	future := childTestTime.Add(ReplayRetention + time.Hour)
	acceptTest(t, q, "maintenance", future)
	if _, err = q.ByKey(t.Context(), req.Key); err != nil {
		t.Fatal("source-actionable key expired", err)
	}
	var before, after int64
	used := func() int64 {
		var pages, free, size int64
		if err := q.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
			t.Fatal(err)
		}
		if err := q.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
			t.Fatal(err)
		}
		if err := q.db.QueryRow(`PRAGMA page_size`).Scan(&size); err != nil {
			t.Fatal(err)
		}
		return (pages - free) * size
	}
	before = used()
	replay := []byte(`{"exact":"opaque host digest"}`)
	if err = q.CompactHumanRestart(t.Context(), r.ID, req.Plan, replay, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatal("compacted before replay window", err)
	}
	if err = q.CompactHumanRestart(t.Context(), r.ID, req.Plan, replay, future); err != nil {
		t.Fatal(err)
	}
	after = used()
	if before-after < 1<<20 {
		t.Fatal("large plan bytes not released", before, after)
	}
	c, err := q.HumanRestartReplay(t.Context(), r.ID)
	if err != nil || !bytes.Equal(c.Replay, replay) || c.Disposition != "cancelled" {
		t.Fatal(c, err)
	}
	if _, _, err = q.AcceptHumanRestart(t.Context(), req, future); !errors.Is(err, ErrConflict) {
		t.Fatal("compact key revived", err)
	}
	if err = q.ForgetRetiredHumanRestart(t.Context(), r.ID, req.Gaggle, req.SourceRun); !errors.Is(err, ErrTransition) {
		t.Fatal("unmarked source released tombstone", err)
	}
	if err = q.MarkHumanRestartSourceRetiring(t.Context(), req.Gaggle, req.SourceRun, future); err != nil {
		t.Fatal(err)
	}
	if err = q.ForgetRetiredHumanRestart(t.Context(), r.ID, "foreign", req.SourceRun); !errors.Is(err, ErrTransition) {
		t.Fatal("foreign source released key", err)
	}
	if err = q.ForgetRetiredHumanRestart(t.Context(), r.ID, req.Gaggle, req.SourceRun); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ByKey(t.Context(), req.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
}
