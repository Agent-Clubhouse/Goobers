package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// noWorkStreakRepo is the repository every fixture in this file claims against.
var noWorkStreakRepo = providers.RepositoryRef{
	Provider: providers.ProviderGitHub, Owner: "acme", Name: "web",
}

// seedNoWorkStreakRun builds an instance layout holding one claimed item and a
// run journal whose implement stage finished with the given status.
//
// reason is written into the stage's Outputs under the same key deterministic
// stages use, so the fixture exercises the real extraction path rather than a
// bespoke one.
func seedNoWorkStreakRun(t *testing.T, runID, itemID, status, reason string) instance.Layout {
	t.Helper()
	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim(itemID, runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	seedItemRepositoryForTest(t, l, runID, itemID, noWorkStreakRepo)

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	outputs := map[string]any{}
	if reason != "" {
		outputs["noWorkReason"] = reason
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Status: status, Outputs: outputs,
	}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	return l
}

// parkCall returns the single provider call that swapped labels, or nil.
func parkCall(calls []providers.UpdateWorkItemRequest) *providers.UpdateWorkItemRequest {
	for i := range calls {
		if len(calls[i].AddLabels) > 0 {
			return &calls[i]
		}
	}
	return nil
}

// TestRepeatedNoWorkParksItemAtThreshold is the #5379 regression: an item whose
// implement stage keeps answering no-work must leave the ready pool instead of
// being re-offered forever.
func TestRepeatedNoWorkParksItemAtThreshold(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := seedNoWorkStreakRun(t, "run-nowork", "5369", string(apiv1.ResultNoWork), "issue is a CI flake, no code change applies")

	for i := 1; i <= noWorkStreakThreshold; i++ {
		if err := settleNoWorkStreak(context.Background(), fake, l, "run-nowork", "implement", ""); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
		if i < noWorkStreakThreshold && parkCall(fake.calls) != nil {
			t.Fatalf("parked early, after %d of %d verdicts", i, noWorkStreakThreshold)
		}
	}

	call := parkCall(fake.calls)
	if call == nil {
		t.Fatalf("no park after %d consecutive no-work verdicts", noWorkStreakThreshold)
	}
	if !slices.Contains(call.AddLabels, providers.LabelNeedsHuman) {
		t.Fatalf("AddLabels = %v, want %s", call.AddLabels, providers.LabelNeedsHuman)
	}
	if !slices.Contains(call.RemoveLabels, providers.LabelReady) {
		t.Fatalf("RemoveLabels = %v, want %s", call.RemoveLabels, providers.LabelReady)
	}
	if !strings.Contains(call.Comment, "issue is a CI flake") {
		t.Fatalf("park comment does not quote the recorded reason:\n%s", call.Comment)
	}
}

// TestProductiveCompletionResetsNoWorkStreak proves a run that actually did
// work clears the streak, so an item that alternates no-work and productive
// verdicts is never parked.
func TestProductiveCompletionResetsNoWorkStreak(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	noWork := seedNoWorkStreakRun(t, "run-nowork", "5369", string(apiv1.ResultNoWork), "nothing to do")

	for i := 0; i < noWorkStreakThreshold-1; i++ {
		if err := settleNoWorkStreak(context.Background(), fake, noWork, "run-nowork", "implement", ""); err != nil {
			t.Fatalf("settle no-work %d: %v", i, err)
		}
	}
	record, err := loadNoWorkStreakRecord(context.Background(), noWork, noWorkStreakRepo, "5369")
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if record.Count != noWorkStreakThreshold-1 {
		t.Fatalf("count = %d, want %d", record.Count, noWorkStreakThreshold-1)
	}

	// A productive run on the same item, in the same instance.
	seedProductiveRunForTest(t, noWork, "run-productive", "5369", "run-nowork")
	if err := settleNoWorkStreak(context.Background(), fake, noWork, "run-productive", "implement", ""); err != nil {
		t.Fatalf("settle productive: %v", err)
	}
	record, err = loadNoWorkStreakRecord(context.Background(), noWork, noWorkStreakRepo, "5369")
	if err != nil {
		t.Fatalf("load record after reset: %v", err)
	}
	if record.Count != 0 {
		t.Fatalf("productive completion left count = %d, want 0", record.Count)
	}
	if parkCall(fake.calls) != nil {
		t.Fatal("productive completion must not park the item")
	}
}

// TestNoWorkStreakSurvivesRestart proves the counter is durable: a fresh
// process reading the same instance root sees the accumulated count, so the
// loop cannot be reset by a daemon restart.
func TestNoWorkStreakSurvivesRestart(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := seedNoWorkStreakRun(t, "run-nowork", "5369", string(apiv1.ResultNoWork), "nothing to do")

	if err := settleNoWorkStreak(context.Background(), fake, l, "run-nowork", "implement", ""); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// A new Layout over the same root is what a restarted daemon constructs.
	restarted := instance.NewLayout(l.Root)
	record, err := loadNoWorkStreakRecord(context.Background(), restarted, noWorkStreakRepo, "5369")
	if err != nil {
		t.Fatalf("load after restart: %v", err)
	}
	if record.Count != 1 {
		t.Fatalf("count after restart = %d, want 1", record.Count)
	}
	if record.Reason != "nothing to do" {
		t.Fatalf("reason after restart = %q, want the recorded rationale", record.Reason)
	}
}

// TestNoWorkParkRetainsEarlierReasonWhenVerdictRecordsNone proves a later
// verdict that journals no rationale does not erase one an earlier verdict
// recorded — the park comment stays informative.
func TestNoWorkParkRetainsEarlierReasonWhenVerdictRecordsNone(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	withReason := seedNoWorkStreakRun(t, "run-reasoned", "5369", string(apiv1.ResultNoWork), "flake, not a code defect")
	if err := settleNoWorkStreak(context.Background(), fake, withReason, "run-reasoned", "implement", ""); err != nil {
		t.Fatalf("settle reasoned: %v", err)
	}
	seedNoWorkRunInLayout(t, withReason, "run-silent", "5369", "", "run-reasoned")
	for i := 0; i < noWorkStreakThreshold-1; i++ {
		if err := settleNoWorkStreak(context.Background(), fake, withReason, "run-silent", "implement", ""); err != nil {
			t.Fatalf("settle silent %d: %v", i, err)
		}
	}
	call := parkCall(fake.calls)
	if call == nil {
		t.Fatal("expected a park at threshold")
	}
	if !strings.Contains(call.Comment, "flake, not a code defect") {
		t.Fatalf("park comment lost the earlier reason:\n%s", call.Comment)
	}
}

// TestNoWorkTerminalIgnoresSuccessfulStages guards the classification itself:
// only a literal no-work stage status counts, so an ordinary successful run is
// treated as productive.
func TestNoWorkTerminalIgnoresSuccessfulStages(t *testing.T) {
	l := seedNoWorkStreakRun(t, "run-ok", "5369", string(apiv1.ResultSuccess), "")
	_, isNoWork, err := noWorkTerminalForRun(l, "run-ok", "implement")
	if err != nil {
		t.Fatalf("noWorkTerminalForRun: %v", err)
	}
	if isNoWork {
		t.Fatal("a successful stage must not be classified as a no-work terminal")
	}
}

// TestNoWorkStreakResetIsIdempotent proves a retried terminal notification for
// a productive run does not churn the plane or error.
func TestNoWorkStreakResetIsIdempotent(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := seedNoWorkStreakRun(t, "run-ok", "5369", string(apiv1.ResultSuccess), "")
	for i := 0; i < 3; i++ {
		if err := settleNoWorkStreak(context.Background(), fake, l, "run-ok", "implement", ""); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	record, err := loadNoWorkStreakRecord(context.Background(), l, noWorkStreakRepo, "5369")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if record.Count != 0 {
		t.Fatalf("count = %d, want 0", record.Count)
	}
}

// reclaimForRun moves an item's claim from whichever run currently holds it to
// runID, which is what the scheduler does between two consecutive claims of the
// same item: the terminal releases, the next tick re-claims.
func reclaimForRun(t *testing.T, l instance.Layout, runID, itemID, priorRunID string) {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if err := ledger.Release(itemID, priorRunID); err != nil {
		t.Fatalf("release %s from %s: %v", itemID, priorRunID, err)
	}
	if ok, holder, err := ledger.Claim(itemID, runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("re-claim %s for %s: ok=%v holder=%q err=%v", itemID, runID, ok, holder, err)
	}
}

// seedNoWorkRunInLayout adds another run journal to an existing layout, taking
// over the item's claim from priorRunID.
func seedNoWorkRunInLayout(t *testing.T, l instance.Layout, runID, itemID, reason, priorRunID string) {
	t.Helper()
	seedItemRepositoryForTest(t, l, runID, itemID, noWorkStreakRepo)
	reclaimForRun(t, l, runID, itemID, priorRunID)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation"}, nil)
	if err != nil {
		t.Fatalf("journal.Create %s: %v", runID, err)
	}
	outputs := map[string]any{}
	if reason != "" {
		outputs["noWorkReason"] = reason
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement",
		Status: string(apiv1.ResultNoWork), Outputs: outputs,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// seedProductiveRunForTest adds a run whose implement stage succeeded, taking
// over the item's claim from priorRunID.
func seedProductiveRunForTest(t *testing.T, l instance.Layout, runID, itemID, priorRunID string) {
	t.Helper()
	seedItemRepositoryForTest(t, l, runID, itemID, noWorkStreakRepo)
	reclaimForRun(t, l, runID, itemID, priorRunID)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultSuccess),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// seedRunWithEvents builds a run journal from explicit events, so a test can
// describe a multi-stage or fan-out run exactly.
func seedRunWithEvents(t *testing.T, l instance.Layout, runID string, events []journal.Event) {
	t.Helper()
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation"}, nil)
	if err != nil {
		t.Fatalf("journal.Create %s: %v", runID, err)
	}
	for i, ev := range events {
		if err := jr.Append(ev); err != nil {
			t.Fatalf("append event %d to %s: %v", i, runID, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close %s: %v", runID, err)
	}
}

// TestParallelBranchNoWorkDoesNotCountAsRunVerdict is the regression for the
// fan-out misclassification: branch stages append to the SAME run journal, and
// a branch that returns no-work ends only that branch while its siblings keep
// producing. Counting that as the run's verdict would park a healthy item.
func TestParallelBranchNoWorkDoesNotCountAsRunVerdict(t *testing.T) {
	l := seedNoWorkStreakRun(t, "run-seed", "5369", string(apiv1.ResultSuccess), "")
	seedRunWithEvents(t, l, "run-fanout", []journal.Event{
		{Type: journal.EventStageFinished, Stage: "security", Branch: 1, Status: string(apiv1.ResultNoWork)},
		{Type: journal.EventStageFinished, Stage: "performance", Branch: 2, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventStageFinished, Stage: "collate", Branch: 0, Status: string(apiv1.ResultSuccess)},
	})
	_, isNoWork, err := noWorkTerminalForRun(l, "run-fanout", "collate")
	if err != nil {
		t.Fatalf("noWorkTerminalForRun: %v", err)
	}
	if isNoWork {
		t.Fatal("a branch's no-work verdict must not be treated as the run's verdict")
	}
}

// TestStaleEarlierNoWorkDoesNotCountWhenRunEndedProductively is the regression
// for the #5107 repass: a no-work event stays in the journal forever, so a run
// that later produced a pull request must not be classified by it.
func TestStaleEarlierNoWorkDoesNotCountWhenRunEndedProductively(t *testing.T) {
	l := seedNoWorkStreakRun(t, "run-seed", "5369", string(apiv1.ResultSuccess), "")
	seedRunWithEvents(t, l, "run-repass", []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultNoWork)},
		{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventStageFinished, Stage: "close-out", Status: string(apiv1.ResultSuccess)},
	})
	_, isNoWork, err := noWorkTerminalForRun(l, "run-repass", "close-out")
	if err != nil {
		t.Fatalf("noWorkTerminalForRun: %v", err)
	}
	if isNoWork {
		t.Fatal("a stale earlier no-work must not classify a run that ended productively")
	}
}

// TestBatchClaimNoWorkParksNothing is the regression for the curation batch:
// a run holding many claims gives no basis to attribute its no-work verdict to
// any single item, so none may be parked.
func TestBatchClaimNoWorkParksNothing(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := seedNoWorkStreakRun(t, "run-batch", "101", string(apiv1.ResultNoWork), "nothing to curate")
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	for _, id := range []string{"102", "103"} {
		if ok, _, err := ledger.Claim(id, "run-batch", "backlog-curation", time.Hour); err != nil || !ok {
			t.Fatalf("seed batch claim %s: ok=%v err=%v", id, ok, err)
		}
		seedItemRepositoryForTest(t, l, "run-batch", id, noWorkStreakRepo)
	}

	for i := 0; i < noWorkStreakThreshold+1; i++ {
		if err := settleNoWorkStreak(context.Background(), fake, l, "run-batch", "implement", ""); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	if call := parkCall(fake.calls); call != nil {
		t.Fatalf("a batch-claim run must park nothing, but parked %s#%s", call.Repository.Name, call.ID)
	}
	for _, id := range []string{"101", "102", "103"} {
		record, err := loadNoWorkStreakRecord(context.Background(), l, noWorkStreakRepo, id)
		if err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		if record.Count != 0 {
			t.Fatalf("item %s accrued count %d from a batch-claim run, want 0", id, record.Count)
		}
	}
}

// TestUnreadableJournalLeavesStreakIntact is the regression for the erasing
// reset: a transient read failure must not discard accumulated evidence.
func TestUnreadableJournalLeavesStreakIntact(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := seedNoWorkStreakRun(t, "run-nowork", "5369", string(apiv1.ResultNoWork), "nothing to do")
	if err := settleNoWorkStreak(context.Background(), fake, l, "run-nowork", "implement", ""); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// A run id with no journal on disk stands in for an unreadable journal.
	if err := settleNoWorkStreak(context.Background(), fake, l, "run-vanished", "implement", ""); err != nil {
		t.Fatalf("settle vanished: %v", err)
	}
	record, err := loadNoWorkStreakRecord(context.Background(), l, noWorkStreakRepo, "5369")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if record.Count != 1 {
		t.Fatalf("unreadable journal changed the streak to %d, want it left at 1", record.Count)
	}
}

// loadNoWorkStreakRecord reads an item's authoritative repeated-no-work record.
//
// This lives in the test file because production no longer needs it: the park
// path takes its count AND its reason from the single compare-and-swap in
// incrementNoWorkStreak, precisely so the two cannot come from different
// versions of the record. Keeping a reader in the binary that nothing calls
// would be dead code; the assertions below still need one.
func loadNoWorkStreakRecord(
	ctx context.Context,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
) (noWorkStreakRecord, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return noWorkStreakRecord{}, fmt.Errorf("open no-work-streak state: %w", err)
	}
	key := noWorkStreakKey(repo, itemID)
	value, err := store.Get(ctx, noWorkStreakStateKey(key))
	if err != nil {
		return noWorkStreakRecord{}, fmt.Errorf("read no-work-streak state for %s#%s: %w", repo.Name, itemID, err)
	}
	return decodeNoWorkStreakRecord(value, key)
}
