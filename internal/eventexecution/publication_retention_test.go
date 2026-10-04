package eventexecution

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestPublicationSettlementRequiresJoinedOwnerAndExclusiveTerminalJournal(t *testing.T) {
	now := time.Now().UTC()
	id := journal.RunIdentity{RunID: "0af7651916cd43dd8448eb211c80319c", Workflow: "producer", WorkflowVersion: 1, Gaggle: "one", ConfigGeneration: "generation", Trigger: journal.Trigger{Kind: journal.TriggerManual}}
	writer, err := journal.Create(t.TempDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	for _, event := range []journal.Event{{Type: journal.EventRunStarted}, {Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}} {
		if err = writer.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	terminalSeq := writer.Seq()
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	envelope, err := (eventing.Publication{Type: "done", OccurrenceKey: "one"}).Envelope(id.Gaggle, id.RunID, "visit")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = queue.BeginEventPublication(t.Context(), triggerqueue.EventPublication{ID: envelope.ID, ConfigGeneration: id.ConfigGeneration, Occurrence: "visit", Acceptance: triggerqueue.EventAcceptance{Producer: eventing.Producer{Gaggle: id.Gaggle, RunID: id.RunID, RootID: id.RunID, Binding: "workflow:producer", Actor: "workflow:" + id.RunID, Stage: "publish"}, Envelope: envelope.JSON, Plan: eventing.Plan{Revision: "catalog"}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	owns := true
	service := &Service{Queue: queue, Now: func() time.Time { return now }, RunDirectory: func(context.Context, string) (string, error) { return writer.Dir(), nil }, AcquireTerminal: func(string) (func(), bool) { return func() {}, owns }}
	check := func(want bool) {
		t.Helper()
		if _, err := service.SettlePublicationSweep(t.Context(), ""); err != nil {
			t.Fatal(err)
		}
		page, err := queue.EventPublicationPage(t.Context(), "", 100)
		if err != nil || len(page) != 1 || (!page[0].SettledAt.IsZero()) != want {
			t.Fatal(page, err)
		}
		if want && page[0].TerminalSequence != terminalSeq {
			t.Fatal("terminal sequence differs")
		}
	}
	check(false) // A terminal event is insufficient while a writer still owns it.
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	owns = false
	check(false) // The registry still excludes an unjoined owner.
	owns = true
	check(true)
}
