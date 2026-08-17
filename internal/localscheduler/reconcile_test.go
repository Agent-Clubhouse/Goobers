package localscheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readprobe"
)

func TestActiveRunCountsReconciliation(t *testing.T) {
	runsDir := t.TempDir()

	newRun := func(runID, workflow string, finish bool) {
		t.Helper()
		run, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: runID, Workflow: workflow, WorkflowVersion: 1, Gaggle: "g",
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if finish {
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
		}
		_ = run.Close()
	}

	newRun("0af7651916cd43dd8448eb211c80319a", "implement", false) // active
	newRun("0af7651916cd43dd8448eb211c80319b", "implement", false) // active
	newRun("0af7651916cd43dd8448eb211c80319c", "implement", true)  // terminal, not counted
	newRun("0af7651916cd43dd8448eb211c80319d", "nominate", false)  // active, different workflow

	counts, err := ActiveRunCounts(runsDir)
	if err != nil {
		t.Fatalf("ActiveRunCounts: %v", err)
	}
	if counts["implement"] != 2 {
		t.Errorf("implement active count = %d, want 2", counts["implement"])
	}
	if counts["nominate"] != 1 {
		t.Errorf("nominate active count = %d, want 1", counts["nominate"])
	}
}

func TestActiveRunCountsMissingDir(t *testing.T) {
	counts, err := ActiveRunCounts(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing runs dir should not error (fresh instance): %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("expected empty counts, got %v", counts)
	}
}

func TestVisitActiveRunsChecksCancellationBetweenJournals(t *testing.T) {
	runsDir := t.TempDir()
	for _, runID := range []string{"active-a", "active-b"} {
		run, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: runID, Workflow: "implement", WorkflowVersion: 1, Gaggle: "g",
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	visited := 0
	err := visitActiveRunsContext(ctx, runsDir, func(journal.RunIdentity) {
		visited++
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("visitActiveRunsContext error = %v, want context.Canceled", err)
	}
	if visited != 1 {
		t.Fatalf("visited %d journals after cancellation, want 1", visited)
	}
}

func TestActiveRunCountsRejectsFutureJournalSchema(t *testing.T) {
	runsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(runsDir, "00-unrelated"), 0o755); err != nil {
		t.Fatal(err)
	}
	futureDir := filepath.Join(runsDir, "01-future")
	if err := os.Mkdir(futureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	schema, err := json.Marshal(journal.SchemaInfo{
		Version:       journal.CurrentSchemaVersion + 1,
		MinimumBinary: "v2.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(futureDir, "schema.json"), schema, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = ActiveRunCounts(runsDir)
	if err == nil {
		t.Fatal("active-run scan accepted a future journal schema")
	}
	for _, want := range []string{"01-future", "version 2", "supported version 1", "minimum binary is v2.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ActiveRunCounts error %q does not contain %q", err, want)
		}
	}
}

func TestActiveRunCountsUsesEventLogPhase(t *testing.T) {
	runsDir := t.TempDir()

	for _, tc := range []struct {
		runID string
		state []byte
	}{
		{
			runID: "stale-terminal",
			state: mustJSON(t, journal.State{
				Schema: journal.StateSchema, RunID: "stale-terminal",
				Phase: journal.PhaseRunning, MachineState: "local-ci",
			}),
		},
		{runID: "unreadable-state", state: []byte("{not json")},
	} {
		run, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: tc.runID, Workflow: "implement", WorkflowVersion: 1, Gaggle: "g",
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runsDir, tc.runID, "state.json"), tc.state, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for restart := 0; restart < 3; restart++ {
		counts, err := ActiveRunCounts(runsDir)
		if err != nil {
			t.Fatalf("restart %d: ActiveRunCounts: %v", restart, err)
		}
		if got := counts["implement"]; got != 0 {
			t.Fatalf("restart %d: implement active count = %d, want 0", restart, got)
		}
	}
}

func TestActiveRunCountsSurfacesUnreadableEventLog(t *testing.T) {
	runsDir := t.TempDir()
	const runID = "corrupt-events"
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "implement", WorkflowVersion: 1, Gaggle: "g",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID, "events.jsonl"), []byte("{not json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = ActiveRunCounts(runsDir)
	if err == nil {
		t.Fatal("ActiveRunCounts succeeded with an unreadable event log")
	}
	if !strings.Contains(err.Error(), runID) {
		t.Fatalf("error = %q, want run ID %q", err, runID)
	}
}

func TestReleaseReconciledOnlyReleasesMatchingRun(t *testing.T) {
	runsDir := t.TempDir()
	newRun := func(runID string, terminal bool) {
		t.Helper()
		run, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: runID, Workflow: "implement", WorkflowVersion: 1, Gaggle: "g",
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if terminal {
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	newRun("running", false)
	newRun("terminal", true)

	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	sched := New(nil, log)
	if err := sched.Reconcile(runsDir, time.Now()); err != nil {
		t.Fatal(err)
	}
	identity := WorkflowIdentity{Gaggle: "g", Workflow: "implement"}
	if got := sched.conditions.ActiveWorkflow(identity); got != 1 {
		t.Fatalf("active count after reconcile = %d, want 1", got)
	}

	sched.ReleaseReconciled("terminal", "implement")
	if got := sched.conditions.ActiveWorkflow(identity); got != 1 {
		t.Fatalf("active count after terminal release = %d, want running run's slot preserved", got)
	}

	sched.ReleaseReconciled("running", "implement")
	if got := sched.conditions.ActiveWorkflow(identity); got != 0 {
		t.Fatalf("active count after running release = %d, want 0", got)
	}
}

func TestReconcileScopesActiveRunsAcrossGaggles(t *testing.T) {
	runsDir := t.TempDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "alpha-active", Gaggle: "alpha", Workflow: "deploy", WorkflowVersion: 1,
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	alpha := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	beta := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched := New([]WorkflowEntry{
		{Gaggle: "alpha", Workflow: "deploy", Signals: []string{"release"}, Starter: alpha},
		{Gaggle: "beta", Workflow: "deploy", Signals: []string{"release"}, Starter: beta},
	}, log)
	if err := sched.Reconcile(runsDir, time.Now()); err != nil {
		t.Fatal(err)
	}

	runIDs := sched.Signal(context.Background(), "release", time.Now())
	if len(runIDs) != 1 {
		t.Fatalf("signal run IDs = %v, want only beta admitted", runIDs)
	}
	waitForCount(t, beta.count, 1)
	sched.Wait()
	if got := alpha.count(); got != 0 {
		t.Fatalf("alpha starts = %d, want 0 while its reconciled run holds the slot", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestActiveRunCountsReadsBoundedJournalBytes is #2755's acceptance bar, stated
// in work rather than in wall time: the daemon's boot reconciliation must not
// read history it will never count.
//
// A latency assertion cannot tell "fast because it is bounded" from "fast
// because the fixture is small", and that is exactly how this path regressed to
// O(all runs ever) — 54,333 run journals opened and parsed at boot on the
// self-hosting instance — with no test noticing. So the assertion is on bytes
// read, via readprobe. Opens deliberately stay unbounded here: every run
// directory is still visited, and that is fine. It is the bytes per visit that
// had to stop scaling with the run's history.
func TestActiveRunCountsReadsBoundedJournalBytes(t *testing.T) {
	runsDir := t.TempDir()
	const (
		terminalRuns  = 8
		fillerRecords = 60
		fillerBytes   = 4096
	)

	newRun := func(runID string, terminal bool) {
		t.Helper()
		run, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: runID, Workflow: "implement", WorkflowVersion: 1, Gaggle: "g",
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if terminal {
			// Written straight to the log rather than through Append: the point of
			// the fixture is a long history behind the decisive record, and paying
			// a checkpoint and an fsync per filler record buys nothing here.
			appendFillerRecords(t, filepath.Join(runsDir, runID), fillerRecords, fillerBytes)
			if err := run.Append(journal.Event{
				Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}

	for i := range terminalRuns {
		newRun(fmt.Sprintf("0af7651916cd43dd8448eb211c8031%02d", i), true)
	}
	newRun("0af7651916cd43dd8448eb211c8031ff", false)

	readprobe.Enable()
	t.Cleanup(readprobe.Disable)
	before := readprobe.Take()
	counts, err := ActiveRunCounts(runsDir)
	if err != nil {
		t.Fatalf("ActiveRunCounts: %v", err)
	}
	work := readprobe.Take().Sub(before)

	if counts["implement"] != 1 {
		t.Fatalf("implement active count = %d, want 1", counts["implement"])
	}
	if work.ActiveScanDirs != terminalRuns+1 {
		t.Fatalf("scanned %d directories, want %d", work.ActiveScanDirs, terminalRuns+1)
	}

	// The terminal journals are the history that must not be re-read. Each is
	// decided from its tail, so the scan's bytes are dominated by the one
	// never-finished run — which has no decisive record and so is legitimately
	// read to the start.
	terminalBytes := journalBytes(t, runsDir) - runJournalBytes(t, runsDir, "0af7651916cd43dd8448eb211c8031ff")
	if terminalBytes < 8*fillerRecords*fillerBytes {
		t.Fatalf("fixture holds only %d bytes of terminal history; too small to show a bound", terminalBytes)
	}
	if int64(work.RunPhaseBytes) > terminalBytes/4 {
		t.Errorf("phase reconstruction read %d bytes against %d bytes of terminal history; "+
			"want a small fraction of it (the scan is reading history again)",
			work.RunPhaseBytes, terminalBytes)
	}
}

// appendFillerRecords writes n non-decisive records of roughly size bytes each
// directly to a run's event log.
func appendFillerRecords(t *testing.T, runDir string, n, size int) {
	t.Helper()
	filler := strings.Repeat("p", size)
	var buf bytes.Buffer
	for i := range n {
		line, err := json.Marshal(journal.Event{
			Schema: journal.EventSchema,
			Type:   journal.EventStageFinished,
			Target: fmt.Sprintf("pad-%d", i),
			Runner: map[string]any{"filler": filler},
		})
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(filepath.Join(runDir, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// journalBytes is the total size of every run's event log under runsDir.
func journalBytes(t *testing.T, runsDir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			total += runJournalBytes(t, runsDir, e.Name())
		}
	}
	return total
}

func runJournalBytes(t *testing.T, runsDir, runID string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(runsDir, runID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
