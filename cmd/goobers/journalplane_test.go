package main

// journalplane_test.go covers the daemon wiring of the live journal service
// (distributed-state-and-coordination.md §8, DS4/DS5) and the second half of
// §13 acceptance item 3: StalledRunTimeout sees a wedged engine run, because
// live authorship gives the run an on-disk journal the existing sweep
// already knows how to read.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

func liveOpenBatch(runID, gaggle string, at time.Time) livejournal.EmitRequest {
	return livejournal.EmitRequest{
		RunID:  runID,
		Gaggle: gaggle,
		Open: &livejournal.OpenHeader{
			Identity: journal.RunIdentity{
				RunID: runID, Workflow: "implementation", WorkflowVersion: 1,
				WorkflowDigest: "sha256:abc", Gaggle: gaggle,
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
			},
			Graph:      json.RawMessage(`{"nodes":[]}`),
			Definition: json.RawMessage(`{"name":"implementation"}`),
		},
		Ops: []livejournal.Op{
			{Kind: livejournal.OpAppend, Key: runID + "|0||0|0", Time: at, Event: &journal.Event{
				Type: journal.EventRunStarted, Status: string(journal.PhaseRunning),
			}},
			{Kind: livejournal.OpAppend, Key: runID + "|0|implement|1|0", Time: at.Add(time.Second), Event: &journal.Event{
				Type: journal.EventStageStarted, Stage: "implement", Attempt: 1,
			}},
		},
	}
}

// TestSweepStalledRunsTerminalizesWedgedLiveEngineRun is §13 item 3's second
// half: an engine run whose journal is live-authored (DS4) is visible to the
// StalledRunTimeout sweep MID-RUN — under the superseded closed-run
// projection model the same wedged run had no journal at all until terminal
// and was undetectable. The writer's idle-close releases the run-dir lock so
// the sweep (whose silence threshold is necessarily longer than the idle
// window) can terminalize, exactly the interlock the daemon wiring runs each
// projection tick.
func TestSweepStalledRunsTerminalizesWedgedLiveEngineRun(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return layout.RunsDir(), true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)

	const runID = "wedged-engine-run"
	if _, err := writer.Emit(context.Background(), liveOpenBatch(runID, "example", now.Add(-2*time.Hour))); err != nil {
		t.Fatalf("emit opening batch: %v", err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), runID, journal.PhaseRunning)

	// The run wedges: no further emissions. The writer's idle-close (the
	// projection loop runs it each tick) releases the journal and its lock.
	if closed := writer.CloseIdle(0); len(closed) != 1 || closed[0] != runID {
		t.Fatalf("CloseIdle = %v, want the wedged run released", closed)
	}

	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{Worktrees: manager, RunsDir: layout.RunsDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := sweepStalledRuns(context.Background(), layout, nil, runRunner, nil, nil, nil, nil, nil, now, 45*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), runID, journal.PhaseEscalated)
}

// TestNewLiveJournalWriterRequiresEngineConfiguration pins the writer's gate
// to the projection loop's own: no engine, no live journal service — and
// with one, emissions land in the configured gaggle's runs directory while
// unknown gaggles are refused.
func TestNewLiveJournalWriterRequiresEngineConfiguration(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "web"}}}}

	writer, err := newLiveJournalWriter(layout, &instance.Config{}, set, nil, nil)
	if err != nil || writer != nil {
		t.Fatalf("writer without engine config = (%v, %v), want (nil, nil)", writer, err)
	}

	// With an engine configured, the writer is wired to the read-model
	// intake — the existing readservice/SSE machinery — so a live run's
	// appends surface mid-flight through the same path local-runner appends
	// do.
	watermarks, err := intake.Open(layout.IntakeDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watermarks.Close() })
	cfg := &instance.Config{Engine: &instance.EngineConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "q"}}
	writer, err = newLiveJournalWriter(layout, cfg, set, watermarks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if writer == nil {
		t.Fatal("engine-configured instance built no live journal writer")
	}
	t.Cleanup(writer.Close)

	at := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	if _, err := writer.Emit(context.Background(), liveOpenBatch("live-wired-run", "web", at)); err != nil {
		t.Fatalf("emit into configured gaggle: %v", err)
	}
	if !journal.Recorded(filepath.Join(layout.ForGaggle("web").RunsDir(), "live-wired-run")) {
		t.Fatal("live journal was not created under the gaggle's runs directory")
	}
	marker, ok, err := watermarks.Get(context.Background(), "live-wired-run")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || marker.SourceSeq == 0 {
		t.Fatalf("read-model intake never observed the mid-run append: marker = %+v, ok = %t", marker, ok)
	}
	if _, err := writer.Emit(context.Background(), liveOpenBatch("stray-run", "unknown", at)); err == nil {
		t.Fatal("emit into an unconfigured gaggle was accepted")
	}
}

// TestLiveJournalDivergenceReporterFilesCodedInstanceEvent: the DS5 channel
// the #2871 parity ledger reads is an instance-journal error event under the
// stable live_journal_divergence code.
func TestLiveJournalDivergenceReporterFilesCodedInstanceEvent(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	if reporter := liveJournalDivergenceReporter(nil); reporter != nil {
		t.Fatal("nil instance log must produce no reporter")
	}
	reporter := liveJournalDivergenceReporter(log)
	reporter("run-diverged", "normative event 3 diverges")

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.Type == journal.EventError && event.RunID == "run-diverged" &&
			event.Error != nil && event.Error.Code == liveJournalDivergenceCode &&
			event.Error.Message == "normative event 3 diverges" {
			found = true
		}
	}
	if !found {
		t.Fatalf("instance journal has no %s event: %+v", liveJournalDivergenceCode, events)
	}
}
