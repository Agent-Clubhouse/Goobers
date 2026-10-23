//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readmodel"
)

func pgTerminalIntent(id string, seq, generation uint64) journal.TerminalIntervention {
	fingerprint := journal.Digest([]byte(id))
	return journal.TerminalIntervention{RequestID: id, Fingerprint: fingerprint, ExpectedTerminalSeq: seq, ExpectedTerminalGeneration: generation, Events: []journal.Event{
		{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "intervention.idempotency", "idempotencyKey": id, "fingerprint": fingerprint, "action": "resume", "stage": "build", "emitKey": id + "-marker"}},
		{Type: journal.EventRunResumed, Target: "build", Action: "resume", Actor: "operator", Runner: map[string]any{"emitKey": id + "-resume"}},
	}}
}

func TestPostgresTerminalInterventionFencesLostResponseAndTakeover(t *testing.T) {
	base := postgresJournalConfigRenew(t, false)
	var role string
	if err := base.Pool.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("role=%s err=%v", role, err)
	}
	old := postgresBackend(t, base)
	ctx := t.Context()
	key := journal.RunKey{RunID: "terminal-takeover", Gaggle: "web"}
	h, o, err := old.CreateRun(ctx, journal.CreateRunRequest{Identity: journal.RunIdentity{RunID: key.RunID, Gaggle: key.Gaggle, Workflow: "wf"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.AppendBatch(ctx, h, o, []journal.Event{{Type: journal.EventRunFinished, Status: "failed"}}); err != nil {
		t.Fatal(err)
	}
	intent := pgTerminalIntent("request", 2, 1)
	receipt, err := old.CommitTerminalIntervention(ctx, h, o, intent)
	if err != nil || receipt.Seq != 4 || receipt.Segment != 2 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	// Lose the response and transfer the controller lease before retrying.
	if err := base.Lease.Release(ctx, base.LiveOwner()); err != nil {
		t.Fatal(err)
	}
	manager, err := lease.New(lease.Config{Pool: base.Pool, InstanceID: base.InstanceID, Name: lease.ControllerName, Holder: "successor"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Release(context.Background(), handle) }()
	next := postgresBackend(t, journalpg.Config{Pool: base.Pool, InstanceID: base.InstanceID, Lease: manager, LiveOwner: func() *lease.Handle { return handle }})
	nh, no, report, err := next.RecoverRun(ctx, key, journal.RunWriterOptions{})
	if err != nil || report.LastSeq != 4 {
		t.Fatalf("recover=%+v err=%v", report, err)
	}
	duplicate, err := next.CommitTerminalIntervention(ctx, nh, no, intent)
	if err != nil || !duplicate.Duplicate || duplicate.Seq != receipt.Seq || duplicate.Segment != 2 {
		t.Fatalf("lost response=%+v err=%v", duplicate, err)
	}
	if _, err := old.CommitTerminalIntervention(ctx, h, o, intent); err == nil {
		t.Fatal("stale owner returned a successful duplicate receipt")
	}
	changed := pgTerminalIntent("request", 2, 1)
	changed.Events[1].Target = "different"
	if _, err := next.CommitTerminalIntervention(ctx, nh, no, changed); !errors.Is(err, journal.ErrInterventionConflict) {
		t.Fatalf("changed payload=%v", err)
	}
	if _, err := next.CommitTerminalIntervention(ctx, nh, no, pgTerminalIntent("other", 2, 1)); !errors.Is(err, journal.ErrInterventionStale) {
		t.Fatalf("other intent=%v", err)
	}
	if _, err := next.AppendBatch(ctx, nh, no, []journal.Event{{Type: journal.EventRunFinished, Status: "escalated"}}); err != nil {
		t.Fatal(err)
	}
	deny := pgTerminalIntent("deny", 5, 2)
	deny.Events = deny.Events[:1]
	deny.Events[0].Runner["kind"] = "escalation.resolution"
	deny.Events[0].Runner["resolution"] = "deny"
	delete(deny.Events[0].Runner, "action")
	delete(deny.Events[0].Runner, "stage")
	denied, err := next.CommitTerminalIntervention(ctx, nh, no, deny)
	if err != nil || denied.Resumed || denied.Segment != 2 || denied.Seq != 6 {
		t.Fatalf("deny=%+v err=%v", denied, err)
	}
	reader, err := next.OpenRun(ctx, journal.RunRef{ID: key.RunID})
	if err != nil {
		t.Fatal(err)
	}
	state, err := reader.State()
	if err != nil || state.Phase != journal.PhaseEscalated {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestPostgresTerminalInterventionMatchesFileProjection(t *testing.T) {
	config := postgresJournalConfig(t)
	pg := postgresBackend(t, config)
	runs := t.TempDir()
	file := journal.NewFileBackend(journal.FileBackendConfig{RunsDir: runs})
	var views [][]journal.Event
	var projections []readmodel.Projection
	fixed := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	identity := journal.RunIdentity{RunID: "terminal-parity", Gaggle: "web", Workflow: "wf"}
	for _, backend := range []journal.Backend{file, pg} {
		h, o, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: identity, Options: journal.RunWriterOptions{Clock: func() time.Time { return fixed }}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.AppendBatch(t.Context(), h, o, []journal.Event{{Type: journal.EventRunFinished, Status: "failed"}}); err != nil {
			t.Fatal(err)
		}
		seam := backend.(journal.TerminalInterventionBackend)
		if _, err := seam.CommitTerminalIntervention(t.Context(), h, o, pgTerminalIntent("resume", 2, 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.AppendBatch(t.Context(), h, o, []journal.Event{{Type: journal.EventStageStarted, Stage: "build", Attempt: 2}, {Type: journal.EventStageFinished, Stage: "build", Attempt: 2, Status: "completed"}, {Type: journal.EventRunFinished, Status: "completed"}}); err != nil {
			t.Fatal(err)
		}
		if err := backend.Retire(t.Context(), h, o); err != nil {
			t.Fatal(err)
		}
		var readers journal.ReaderBackend = pg
		if backend == file {
			readers = journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{runs}})
		}
		reader, err := readers.OpenRun(t.Context(), journal.RunRef{ID: identity.RunID})
		if err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, events)
		projection, err := readmodel.ProjectRunFromJournal(reader, identity, events)
		if err != nil {
			t.Fatal(err)
		}
		projections = append(projections, projection)
	}
	if !reflect.DeepEqual(views[0], views[1]) {
		t.Fatalf("file=%+v PG=%+v", views[0], views[1])
	}
	if !reflect.DeepEqual(projections[0], projections[1]) {
		t.Fatalf("file=%+v PG=%+v", projections[0], projections[1])
	}
}

func TestPostgresTerminalInterventionLiveWriterKeepsOrdinaryEmitsClosed(t *testing.T) {
	config := postgresJournalConfig(t)
	writer, backend, root := postgresLiveWriter(t, config)
	ctx := t.Context()
	const id = "live-terminal"
	start := journal.Event{Type: journal.EventRunStarted, Status: "running"}
	finish := journal.Event{Type: journal.EventRunFinished, Status: "failed"}
	req := livejournal.EmitRequest{RunID: id, Gaggle: "web", Open: &livejournal.OpenHeader{Identity: journal.RunIdentity{RunID: id, Gaggle: "web", Workflow: "wf"}, Graph: []byte(`{"nodes":[]}`), Definition: []byte(`{"name":"wf"}`)}, Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "start", Event: &start}, {Kind: livejournal.OpAppend, Key: "finish", Event: &finish}}}
	first, err := writer.Emit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	intent := livejournal.TerminalInterventionRequest{RunID: id, Gaggle: "web", Transition: pgTerminalIntent("resume", first.Seq, 1)}
	stray := livejournal.EmitRequest{RunID: id, Gaggle: "web", Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "stray", Event: &intent.Transition.Events[1]}}}
	if _, err := writer.Emit(ctx, stray); !errors.Is(err, livejournal.ErrTerminal) {
		t.Fatalf("stray=%v", err)
	}
	accepted, err := writer.CommitTerminalIntervention(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	recovered, err := livejournal.NewWriter(func(string) (string, bool) { return root, true }, livejournal.WithBackend(backend))
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	duplicate, err := recovered.CommitTerminalIntervention(ctx, intent)
	if err != nil || !duplicate.Duplicate || duplicate.Seq != accepted.Seq {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	finish.Status = "completed"
	req.Open = nil
	req.Ops = []livejournal.Op{{Kind: livejournal.OpAppend, Key: "second-finish", Event: &finish}}
	last, err := recovered.Emit(ctx, req)
	if err != nil || !last.Terminal {
		t.Fatalf("second terminal=%+v err=%v", last, err)
	}
	assertNoLocalJournal(t, root)
}

func TestPostgresTerminalInterventionRollsBackTheWholePrefix(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	h, o, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: journal.RunIdentity{RunID: "atomic-intervention", Gaggle: "web", Workflow: "wf"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.AppendBatch(t.Context(), h, o, []journal.Event{{Type: journal.EventRunFinished, Status: "failed"}}); err != nil {
		t.Fatal(err)
	}
	owner := postgresOwnerPool(t, config)
	_, err = owner.Exec(t.Context(), `CREATE FUNCTION control.refuse_test_resume() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.type = 'run.resumed' THEN RAISE EXCEPTION 'injected second append failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER refuse_test_resume BEFORE INSERT ON journal.run_event FOR EACH ROW EXECUTE FUNCTION control.refuse_test_resume()`)
	if err != nil {
		t.Fatal(err)
	}
	intent := pgTerminalIntent("atomic", 2, 1)
	if _, err := backend.CommitTerminalIntervention(t.Context(), h, o, intent); err == nil {
		t.Fatal("injected append failure did not fail")
	}
	var count int
	if err := config.Pool.QueryRow(t.Context(), "SELECT count(*) FROM journal.run_event WHERE run_id=$1", "atomic-intervention").Scan(&count); err != nil || count != 2 {
		t.Fatalf("partial transaction committed: count=%d err=%v", count, err)
	}
	if _, err := owner.Exec(t.Context(), "DROP TRIGGER refuse_test_resume ON journal.run_event"); err != nil {
		t.Fatal(err)
	}
	receipt, err := backend.CommitTerminalIntervention(t.Context(), h, o, intent)
	if err != nil || receipt.Seq != 4 || receipt.Duplicate {
		t.Fatalf("retry=%+v err=%v", receipt, err)
	}
}
