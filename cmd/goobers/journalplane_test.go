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
	"github.com/goobers/goobers/internal/blobstore"
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
			// Driver mirrors production: the live journal plane's only emitter
			// is internal/engine's runJournal, whose identity carries
			// journal.DriverEngine from the first emit. A fixture without it
			// is a shape nothing can author, and every sweep/resume assertion
			// built on it would test the runner-driven path while calling
			// itself an engine run.
			Identity: journal.RunIdentity{
				RunID: runID, Workflow: "implementation", WorkflowVersion: 1,
				WorkflowDigest: "sha256:abc", Gaggle: gaggle,
				Driver:  journal.DriverEngine,
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

// TestSweepStalledRunsCancelsWedgedLiveEngineRun is §13 item 3's second half:
// an engine run whose journal is live-authored (DS4) is visible to the
// StalledRunTimeout sweep MID-RUN — under the superseded closed-run
// projection model the same wedged run had no journal at all until terminal
// and was undetectable. The writer's idle-close releases the run-dir lock so
// the sweep (whose silence threshold is necessarily longer than the idle
// window) can act on it, exactly the interlock the daemon wiring runs each
// projection tick.
//
// What the sweep does with it is the guard: the live writer authored the run
// with driver: engine, so the settlement is a CancelWorkflow, not a terminal
// event forged into a journal whose workflow is still executing. This is the
// only test that carries a live-journal-AUTHORED engine run all the way into
// the sweep — enginerunguards_test.go's sweep cases build their journals with
// journal.Create.
func TestSweepStalledRunsCancelsWedgedLiveEngineRun(t *testing.T) {
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
	fake := &fakeEngineWorkflows{}
	if err := sweepStalledRuns(
		context.Background(), layout, nil, runRunner, &engineRunGuards{client: fake}, nil,
		nil, nil, nil, now, 45*time.Minute, 0,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, cancelled := fake.snapshot(); len(cancelled) != 1 || cancelled[0] != runID {
		t.Fatalf("cancelled = %v, want [%s] — a live-authored engine run must be settled on the engine, not on disk", cancelled, runID)
	}
	assertWatchdogPhase(t, layout.RunsDir(), runID, journal.PhaseRunning)
}

// TestNewLiveJournalWriterRequiresEngineConfiguration pins the writer's gate
// to the projection loop's own: no engine, no live journal service — and
// with one, emissions land in the configured gaggle's runs directory while
// unknown gaggles are refused.
func TestNewLiveJournalWriterRequiresEngineConfiguration(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "web"}}}}

	writer, err := newLiveJournalWriter(layout, &instance.Config{}, set, nil, nil, nil)
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
	writer, err = newLiveJournalWriter(layout, cfg, set, watermarks, nil, nil)
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

// liveSpanBatch emits one pointer-only span op — the exact shape the engine
// workflow produces at stageFinished for a stage result carrying a transcript
// (internal/engine/journal.go's JournalSpanOp). The emitter never holds the
// bytes; whether they can be adopted is entirely a question of what span
// source the daemon wired into the writer.
func liveSpanBatch(runID, gaggle, digest string, at time.Time) livejournal.EmitRequest {
	return livejournal.EmitRequest{
		RunID:  runID,
		Gaggle: gaggle,
		Ops: []livejournal.Op{{
			Kind: livejournal.OpSpan,
			Key:  runID + "|0|implement|1|1",
			Time: at,
			Span: &livejournal.SpanOp{
				Stage: "implement", Attempt: 1, Name: "implement.transcript",
				DataSchema: "goobers.dev/telemetry/genai-event/v1",
				Ref:        journal.Ref{Digest: digest},
			},
		}},
	}
}

// TestLiveJournalWriterAdoptsSpansFromTheDaemonBlobStore is the daemon-wiring
// seam for #3805. livejournal's own tests already prove a writer holding a
// SpanSource adopts a span; what was broken was that the daemon never handed
// it one, so every pod-executed agentic stage recorded
// error.code=span_unavailable in place of its transcript (measured on five
// live runs).
//
// The test crosses the seam the bug crossed: it calls newLiveJournalWriter —
// the daemon's own constructor — with a real blobstore.Dir, which is what
// cmd/goobers/up.go now passes, and with nil, which is what it passed before.
func TestLiveJournalWriterAdoptsSpansFromTheDaemonBlobStore(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "web"}}}}
	cfg := &instance.Config{Engine: &instance.EngineConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "q"}}

	// The daemon's own blob store, at the layout path up.go constructs it
	// from — and the store a stage pod PUTs its scrubbed transcript into over
	// the blob plane before the workflow emits the pointer-only span op.
	blobs, err := blobstore.NewDir(layout.BlobStoreDir())
	if err != nil {
		t.Fatal(err)
	}
	transcript := []byte(`{"event":"prompt","adapter":"copilot-cli"}`)
	digest := journal.Digest(transcript)
	if err := blobs.Put(context.Background(), digest, transcript); err != nil {
		t.Fatalf("seed blob store: %v", err)
	}

	at := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	runsDir := layout.ForGaggle("web").RunsDir()

	writer, err := newLiveJournalWriter(layout, cfg, set, nil, nil, blobs)
	if err != nil {
		t.Fatal(err)
	}
	if writer == nil {
		t.Fatal("engine-configured instance built no live journal writer")
	}
	const runID = "live-span-run"
	if _, err := writer.Emit(context.Background(), liveOpenBatch(runID, "web", at)); err != nil {
		t.Fatalf("emit opening batch: %v", err)
	}
	if _, err := writer.Emit(context.Background(), liveSpanBatch(runID, "web", digest, at.Add(time.Second))); err != nil {
		t.Fatalf("emit span op: %v", err)
	}
	writer.Close()

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	span := events[len(events)-1]
	if span.Type != journal.EventSpanRecorded || span.Ref == nil || span.Ref.Digest != digest {
		t.Fatalf("daemon-wired writer did not adopt the span; last event = %+v", span)
	}
	got, err := rd.SpanBytes(*span.Ref)
	if err != nil {
		t.Fatalf("SpanBytes: %v", err)
	}
	if string(got) != string(transcript) {
		t.Fatalf("adopted span bytes = %q, want %q", got, transcript)
	}

	// A daemon with no store still degrades softly rather than failing the
	// emit — the pre-#3805 behaviour, and the posture an instance with no
	// blob plane keeps.
	bare, err := newLiveJournalWriter(layout, cfg, set, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	const bareRunID = "live-span-unwired-run"
	if _, err := bare.Emit(context.Background(), liveOpenBatch(bareRunID, "web", at)); err != nil {
		t.Fatalf("emit opening batch: %v", err)
	}
	if _, err := bare.Emit(context.Background(), liveSpanBatch(bareRunID, "web", digest, at.Add(time.Second))); err != nil {
		t.Fatalf("emit span op without a source must not fail: %v", err)
	}
	bareRd, err := journal.OpenRead(filepath.Join(runsDir, bareRunID))
	if err != nil {
		t.Fatal(err)
	}
	bareEvents, err := bareRd.Events()
	if err != nil {
		t.Fatal(err)
	}
	degraded := bareEvents[len(bareEvents)-1]
	if degraded.Type != journal.EventError || degraded.Error == nil ||
		degraded.Error.Code != livejournal.SpanUnavailableErrorCode {
		t.Fatalf("writer with no span source did not degrade softly; last event = %+v", degraded)
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
