package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/prqueue"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestQueueEligibilityReadsVerifiedArtifactWithoutEventReplay(t *testing.T) {
	ctx := context.Background()
	service, layout, machine := fixtureService(t)
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	run, _ := createFixtureRun(t, layout, machine, "queue-run", machine.Def.Name, "goobers", now, journal.Trigger{Kind: journal.TriggerManual}, true)
	t.Cleanup(func() { _ = run.Close() })
	report := prqueue.Report{Version: 1, RepositoryKey: "github|||org|repo|", RunID: "queue-run", Gaggle: "goobers", Workflow: machine.Def.Name, ObservedAt: now, Items: []prqueue.Item{}}
	report.Add(42, prqueue.Escalated)
	data, err := json.Marshal(map[string]any{"queueEligibility": report})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := run.RecordArtifact("select/result", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "select", Status: "no-work", Attempt: 1, Outputs: map[string]any{"queueEligibilityVersion": "1"}, Artifacts: []journal.Ref{ref}}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := readmodel.Open(filepath.Join(layout.Root, readmodel.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service.sources.ReadModel = store
	service.EnableReadModelReads()
	dir := filepath.Join(layout.RunsDir(), "queue-run")
	if err := store.ProjectRunDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	view, err := service.QueueEligibility(ctx, "goobers", machine.Def.Name)
	if err != nil || view.Status != "unavailable" || view.Report != nil {
		t.Fatalf("unready projection trusted: %+v err=%v", view, err)
	}
	if err := store.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	// Only the indexed pointer, identity and blob should be read. Removing the
	// event-log path makes an accidental whole-journal replay fail visibly.
	if err := os.Rename(filepath.Join(dir, "events.jsonl"), filepath.Join(dir, "events.saved")); err != nil {
		t.Fatal(err)
	}
	view, err = service.QueueEligibility(ctx, "goobers", machine.Def.Name)
	if err != nil || view.Status != "observed" || view.Report == nil || view.Report.Items[0].Number != 42 || view.Report.Items[0].Reason != prqueue.Escalated {
		t.Fatalf("bounded report read failed: %+v err=%v", view, err)
	}
	foreign, err := service.QueueEligibility(ctx, "other", machine.Def.Name)
	if err != nil || foreign.Status != "not-observed" || foreign.Report != nil {
		t.Fatalf("cross-gaggle evidence leaked: %+v err=%v", foreign, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ref.Path), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	view, err = service.QueueEligibility(ctx, "goobers", machine.Def.Name)
	if err != nil || view.Status != "unavailable" || view.Report != nil || view.Problem == "" {
		t.Fatalf("tamper became available/empty: %+v err=%v", view, err)
	}
}

func TestQueueEligibilityUnavailableAndInvalidScope(t *testing.T) {
	service, _, _ := fixtureService(t)
	view, err := service.QueueEligibility(context.Background(), "team", "review")
	if err != nil || view.Status != "unavailable" || view.Report != nil {
		t.Fatalf("missing store became empty queue: %+v err=%v", view, err)
	}
	for _, gaggle := range []string{"..", "../team", `..\team`} {
		if _, err := service.QueueEligibility(context.Background(), gaggle, "review"); err == nil {
			t.Fatalf("accepted scope %q", gaggle)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.QueueEligibility(ctx, "team", "review"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
