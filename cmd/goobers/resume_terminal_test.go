package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/workflow"
)

// newStaleTerminalRun hand-constructs a run whose event log durably shows
// run.finished but whose state.json checkpoint still claims {running,
// <machineState>} — #242's crash window (a real crash between the
// run.finished event's fsync and the checkpoint rewrite that follows it in
// the same journal.Append call). Every terminal-phase decision reachable
// from a runs directory (the daemon's resume scan, `run abort`, Resume
// itself) must trust the reconstructed phase here, not this stale
// checkpoint.
func newStaleTerminalRun(t *testing.T, l instance.Layout, runID, workflowName string, finishedPhase journal.RunPhase, staleMachineState string) {
	t.Helper()
	set, report, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatalf("load fixture config: %v (report: %+v)", err, report)
	}
	var gaggle, digest string
	found := false
	for i := range set.Workflows {
		if set.Workflows[i].Name == workflowName {
			m, err := workflow.Compile(workflow.Definition{Name: set.Workflows[i].Name, Version: 1, DSLVersion: set.Workflows[i].DSLVersion, Spec: set.Workflows[i].Spec}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatalf("compile fixture workflow: %v", err)
			}
			gaggle = set.Workflows[i].Spec.Gaggle
			digest = m.Digest()
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("workflow %q not found in fixture config", workflowName)
	}

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: workflowName, WorkflowVersion: 1,
		WorkflowDigest: digest, Gaggle: gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("hand-construct stale-terminal run journal: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(finishedPhase)}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(l.RunsDir(), runID)
	stale := journal.State{
		Schema: journal.StateSchema, RunID: runID,
		Phase: journal.PhaseRunning, MachineState: staleMachineState,
	}
	b, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), b, 0o644); err != nil {
		t.Fatalf("overwrite state.json: %v", err)
	}
}

// TestResumeInterruptedRunsSkipsStaleTerminalCheckpoint is #242's daemon-scan
// acceptance scenario: resumeInterruptedRuns must not spin up a resume
// goroutine for a run whose journal already shows it finished, even though
// its on-disk checkpoint still claims {running, ...}.
func TestResumeInterruptedRunsSkipsStaleTerminalCheckpoint(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	newStaleTerminalRun(t, l, "stale-terminal-1", "default-implement", journal.PhaseCompleted, "local-ci")

	for restart := 0; restart < 3; restart++ {
		counts, err := localscheduler.ActiveRunCounts(l.RunsDir())
		if err != nil {
			t.Fatalf("restart %d: count active runs: %v", restart, err)
		}
		if got := counts["default-implement"]; got != 0 {
			t.Fatalf("restart %d: active count = %d, want 0", restart, got)
		}
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Shutdown(context.Background())
	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		t.Fatal(err)
	}

	var released []string
	release := func(runID, workflow string) {
		released = append(released, workflow)
		sched.ReleaseReconciled(runID, workflow)
	}
	resumed, warned, err := resumeInterruptedRuns(ctx, l, setup.Runner, setup.Machines, setup.GooberDigests, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, setup.Watermarks, release, &wg)
	if err != nil {
		t.Fatal(err)
	}
	if len(warned) != 0 {
		t.Fatalf("warned = %v, want none", warned)
	}
	if len(resumed) != 0 {
		t.Fatalf("resumed = %v, want none — the run's journal already shows it finished despite the stale checkpoint", resumed)
	}
	if len(released) != 1 || released[0] != "default-implement" {
		t.Fatalf("released = %v, want terminal workflow slot released once", released)
	}
	wg.Wait()

	// The checkpoint itself is untouched by the scan (only Recover, which
	// this path never reaches, heals it) — confirms the skip happened via
	// Phase() reconstruction, not by accident.
	rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), "stale-terminal-1"))
	if err != nil {
		t.Fatal(err)
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatal(err)
	}
	if phase != journal.PhaseCompleted {
		t.Fatalf("Phase() = %q, want completed", phase)
	}
}

func TestResumeInterruptedRunsRejectsFutureJournalSchema(t *testing.T) {
	l := instance.NewLayout(t.TempDir()).ForGaggle("g")
	if err := os.MkdirAll(filepath.Join(l.RunsDir(), "00-unrelated"), 0o755); err != nil {
		t.Fatal(err)
	}
	futureDir := filepath.Join(l.RunsDir(), "01-future")
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

	var wg sync.WaitGroup
	_, _, err = resumeInterruptedRuns(
		context.Background(), l, nil, nil, nil, nil, nil, nil, nil, nil,
		func(string, string) {}, &wg,
	)
	if err == nil {
		t.Fatal("resume scan accepted a future journal schema")
	}
	for _, want := range []string{"01-future", "version 2", "supported version 1", "minimum binary is v2.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("resume scan error %q does not contain %q", err, want)
		}
	}
}

func TestResumeScanReleasesClaimsForAlreadyTerminalRun(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "stale-terminal-with-claim"
	newStaleTerminalRun(t, l, runID, "default-implement", journal.PhaseEscalated, "review")
	if err := os.WriteFile(filepath.Join(l.RunsDir(), runID, "state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	counts, err := localscheduler.ActiveRunCounts(l.RunsDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := counts["default-implement"]; got != 0 {
		t.Fatalf("active count = %d, want 0 with unreadable state.json and terminal event log", got)
	}

	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("498", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Shutdown(context.Background())
	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		t.Fatal(err)
	}

	resumed, warned, err := resumeInterruptedRuns(
		context.Background(), l, setup.Runner, setup.Machines, setup.GooberDigests, setup.RepoRefs,
		setup.InstanceLog, setup.Telemetry, setup.RollupDB, setup.Watermarks, sched.ReleaseReconciled, &wg,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 0 || len(warned) != 0 {
		t.Fatalf("resumed=%v warned=%v, want neither for terminal run", resumed, warned)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := reopened.Lookup("498"); ok {
		t.Fatalf("terminal run's claim survived startup reconciliation: %+v", entry)
	}
}

func TestResumeScanFinalizesTerminalRunFromRemovedGaggle(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	removed := l.ForGaggle("removed")
	const runID = "removed-terminal-run"

	jr, err := journal.Create(removed.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "removed",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	claimKey := localscheduler.ClaimKey{Gaggle: "removed", Provider: "github", ExternalID: "498"}
	if ok, _, err := ledger.ClaimScoped(claimKey, runID, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed scoped claim: ok=%v err=%v", ok, err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Shutdown(context.Background())

	var released []string
	resumed, warned, err := resumeInterruptedRunsWithRunners(
		context.Background(), l, setup.Runners, nil, setup.RunnerRegistry, setup.Machines, setup.GooberDigests, setup.RepoRefs,
		setup.InstanceLog, setup.Telemetry, setup.RollupDB, setup.Watermarks,
		func(_ string, workflow string) { released = append(released, workflow) }, &wg,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 0 || len(warned) != 0 {
		t.Fatalf("resumed=%v warned=%v, want neither for terminal run", resumed, warned)
	}
	if len(released) != 1 || released[0] != "default-implement" {
		t.Fatalf("released = %v, want terminal workflow slot released once", released)
	}
	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := reopened.LookupScoped(claimKey); ok {
		t.Fatalf("removed gaggle's terminal claim survived startup cleanup: %+v", entry)
	}
}

// TestRunAbortRejectsStaleTerminalCheckpoint is #242's `run abort` acceptance
// scenario: aborting a run whose journal already shows it finished — even
// though its checkpoint still claims {running, ...} — must be rejected the
// same way aborting a cleanly-terminal run is, not append a second
// run.finished event that flips the recorded terminal phase.
func TestRunAbortRejectsStaleTerminalCheckpoint(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	newStaleTerminalRun(t, l, "stale-terminal-2", "default-implement", journal.PhaseEscalated, "review")

	code, _, stderr := runArgs(t, "run", "abort", "stale-terminal-2", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "already terminal") {
		t.Fatalf("stderr = %q, want \"already terminal\"", stderr)
	}
	if !strings.Contains(stderr, "escalated") {
		t.Fatalf("stderr = %q, want the JOURNALED phase (escalated), not the stale checkpoint's \"running\"", stderr)
	}

	rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), "stale-terminal-2"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var finished int
	for _, e := range events {
		if e.Type == journal.EventRunFinished {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("run.finished count = %d, want exactly 1 — abort must not append a second terminal event onto an already-finished run", finished)
	}
}

// highestJournalSeq mirrors lastJournalSeq's "take the max, not the last
// record" logic (runnerwiring.go) so tests can assert the intake watermark
// against the run's actual highest sequence without hardcoding a number that
// would drift if the journal fixture setup changes shape.
func highestJournalSeq(t *testing.T, dir string) uint64 {
	t.Helper()
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var highest uint64
	for _, e := range events {
		if e.Seq > highest {
			highest = e.Seq
		}
	}
	return highest
}

// TestResumeScanRecordsIntakeWatermarkForTerminalRun is #2190's acceptance
// scenario: a run discovered already-terminal during the daemon's startup
// resume scan took the finalize-and-release branch without ever recording
// its intake watermark, so the read model never learned it had advanced —
// unlike a normal terminal run, which gets there via ingestRunTelemetry.
func TestResumeScanRecordsIntakeWatermarkForTerminalRun(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "stale-terminal-watermark"
	newStaleTerminalRun(t, l, runID, "default-implement", journal.PhaseCompleted, "local-ci")
	wantSeq := highestJournalSeq(t, filepath.Join(l.RunsDir(), runID))

	ctx := context.Background()
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Shutdown(context.Background())
	if setup.Watermarks == nil {
		t.Fatal("setup.Watermarks is nil — cannot assert intake recording")
	}

	resumed, warned, err := resumeInterruptedRuns(ctx, l, setup.Runner, setup.Machines, setup.GooberDigests, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, setup.Watermarks, func(string, string) {}, &wg)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 0 || len(warned) != 0 {
		t.Fatalf("resumed=%v warned=%v, want neither for a terminal run", resumed, warned)
	}
	wg.Wait()

	marker, ok, err := setup.Watermarks.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("intake watermark not recorded for a terminal run discovered at resume scan")
	}
	if marker.SourceSeq != wantSeq {
		t.Fatalf("marker.SourceSeq = %d, want %d (the run's highest journal sequence)", marker.SourceSeq, wantSeq)
	}
}

// TestRunAbortRecordsIntakeWatermark is #2191's acceptance scenario:
// `goobers run abort` advances the journal to a terminal phase just like any
// other terminal run, but the one-shot CLI path never opened the intake
// store, so the dashboard's view of the run was never refreshed.
func TestRunAbortRecordsIntakeWatermark(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "stuck-watermark"

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "no-such-workflow", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jr.SetMachineState("whatever-state")
	if err := jr.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "run", "abort", runID, root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	wantSeq := highestJournalSeq(t, filepath.Join(l.RunsDir(), runID))

	watermarks, err := intake.Open(l.IntakeDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watermarks.Close() }()

	marker, ok, err := watermarks.Get(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("intake watermark not recorded after `run abort`")
	}
	if marker.SourceSeq != wantSeq {
		t.Fatalf("marker.SourceSeq = %d, want %d (the run's highest journal sequence)", marker.SourceSeq, wantSeq)
	}
}
