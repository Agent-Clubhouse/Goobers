package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// newStuckRunWithDigest is newStuckRun with the WF-016 digest pin overridden —
// the on-disk shape of a run that was in flight when the workflow YAML
// changed (#520): non-terminal journal, checkpoint mid-machine, pinned to a
// digest the daemon's compiled machine no longer matches.
func newStuckRunWithDigest(t *testing.T, l instance.Layout, runID, workflowName, digest string) {
	t.Helper()
	set, report, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatalf("load fixture config: %v (report: %+v)", err, report)
	}
	var gaggle string
	found := false
	for i := range set.Workflows {
		if set.Workflows[i].Name == workflowName {
			gaggle = set.Workflows[i].Spec.Gaggle
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
		t.Fatalf("hand-construct stale-digest run journal: %v", err)
	}
	jr.SetMachineState("local-ci")
	if err := jr.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestResumeScanFailsDigestMismatchedRunAndReleasesClaim is #520's
// end-to-end acceptance scenario, the exact live failure: a daemon restart
// after a workflow-YAML change finds an in-flight run pinned to the old
// digest. The WF-016 refusal is correct and must stand (the run is never
// walked) — but per the maintainer ruling it must land the run at the
// canonical FAILED terminal (not aborted — nobody chose to stop it),
// release its claim through the same startup path #526 wired, journal the
// canonical phase (not a raw "error: ..." string) to the instance log, and
// keep the literal "WF-016" string durable in both state.json and the
// journal event. Before #520 the run stayed phase=running forever and the
// claim leaked for its full lease (10 leaked claims in one config-only
// restart, per the issue).
func TestResumeScanFailsDigestMismatchedRunAndReleasesClaim(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "stuck-stale-digest"
	newStuckRunWithDigest(t, l, runID, "default-implement", "sha256:the-old-workflow-shape")

	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("520", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
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

	resumed, warned, err := resumeInterruptedRuns(ctx, l, setup.Runner, setup.Machines, setup.GooberDigests, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, sched.ReleaseReconciled, &wg)
	if err != nil {
		t.Fatal(err)
	}
	if len(warned) != 0 {
		t.Fatalf("warned = %v, want none — the workflow itself resolves fine, only its digest changed", warned)
	}
	if len(resumed) != 1 || resumed[0] != runID {
		t.Fatalf("resumed = %v, want exactly [%s] — the run is non-terminal, so the scan must pick it up (the refusal happens inside Resume)", resumed, runID)
	}
	wg.Wait()

	rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatal(err)
	}
	if phase != journal.PhaseFailed {
		t.Fatalf("run journal phase = %q, want failed — the refusal must reach a canonical terminal phase, not stay running forever", phase)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Reason, "WF-016") {
		t.Fatalf("state.json Reason = %q, want it to contain \"WF-016\" (ruling grepability requirement)", st.Reason)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := reopened.Lookup("520"); ok {
		t.Fatalf("refused run's claim survived: %+v — finalizeTerminal must release it, this is the leak #520 exists to stop", entry)
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var finished journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventRunFinished && ev.RunID == runID {
			finished = ev
		}
	}
	// #710: the instance-log echo now enriches the canonical phase with the
	// refusal's own code (still never the raw, unbounded "error: <goErr>"
	// string #520 exists to prevent) — refuseResume threads FailureCode onto
	// the Result the same way taskOutcome/failTerminal do for a business
	// failure, so this path gets the same visibility fix.
	wantStatus := string(journal.PhaseFailed) + " (resume_refused_digest_mismatch)"
	if finished.Status != wantStatus {
		t.Fatalf("instance-log status = %q, want %q — the canonical phase enriched with its cause, never a raw \"error: ...\" string", finished.Status, wantStatus)
	}
	if finished.Error == nil || finished.Error.Code != "resume_refused_digest_mismatch" {
		t.Fatalf("instance-log run.finished error = %+v, want code resume_refused_digest_mismatch", finished.Error)
	}
}
