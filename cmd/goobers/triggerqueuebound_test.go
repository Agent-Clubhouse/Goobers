package main

import (
	"errors"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func triggerQueueDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scheduler")
	if err := os.MkdirAll(filepath.Join(dir, pendingTriggersDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func countRequests(t *testing.T, schedulerDir string) int {
	t.Helper()
	n, err := countPendingTriggerRequests(filepath.Join(schedulerDir, pendingTriggersDir))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// #4326 AC: "Repeated executions with unchanged daemon state do not create
// additional trigger requests." The incident's automation resubmitted the same
// lane fill every 15 minutes for 59 hours and accumulated 1,177 files.
func TestPriorityTriggerResubmissionCreatesNoAdditionalRequest(t *testing.T) {
	schedulerDir := triggerQueueDir(t)

	first, err := writePriorityTriggerRequest(schedulerDir, "efunhouse", "implementation", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, err := writePriorityTriggerRequest(schedulerDir, "efunhouse", "implementation", "run-1")
		if err != nil {
			t.Fatalf("resubmission %d: %v", i, err)
		}
		// The repeat must resolve to the SAME request, not merely be discarded
		// later by the sweep: the caller's returned id is what it would use to
		// reconcile, so a fresh id per repeat is the defect itself.
		if again != first {
			t.Fatalf("resubmission %d returned id %q, want the original %q", i, again, first)
		}
	}
	if got := countRequests(t, schedulerDir); got != 1 {
		t.Errorf("%d outstanding requests after 51 identical submissions, want 1", got)
	}
}

// #4326 AC: "Five configured lanes produce at most five outstanding fill
// requests in total." The per-identity sweep bound alone permits five EACH
// (25); only write-side idempotency gives five in total.
func TestFiveLanesProduceFiveOutstandingRequests(t *testing.T) {
	schedulerDir := triggerQueueDir(t)
	lanes := []string{"lane-a", "lane-b", "lane-c", "lane-d", "lane-e"}

	// Simulate the incident's cadence: repeated passes over every lane.
	for pass := 0; pass < 20; pass++ {
		for _, lane := range lanes {
			if _, err := writePriorityTriggerRequest(schedulerDir, "efunhouse", lane, "fill"); err != nil {
				t.Fatalf("pass %d lane %s: %v", pass, lane, err)
			}
		}
	}
	if got := countRequests(t, schedulerDir); got != len(lanes) {
		t.Errorf("%d outstanding requests after 20 passes over %d lanes, want %d", got, len(lanes), len(lanes))
	}
}

// #4326 AC: "A test or simulation covering 59 hours of invocations leaves a
// bounded queue." This is the incident replayed at its measured cadence —
// five lanes, every 15 minutes, for 59 hours — against a daemon that never
// sweeps. Before write-side idempotency this produced 1,180 files.
func TestFiftyNineHoursOfLaneFillLeavesABoundedQueue(t *testing.T) {
	schedulerDir := triggerQueueDir(t)
	lanes := []string{"lane-a", "lane-b", "lane-c", "lane-d", "lane-e"}

	invocations := int((59 * time.Hour) / (15 * time.Minute)) // 236
	for i := 0; i < invocations; i++ {
		for _, lane := range lanes {
			if _, err := writePriorityTriggerRequest(schedulerDir, "efunhouse", lane, "fill"); err != nil {
				t.Fatalf("invocation %d lane %s: %v", i, lane, err)
			}
		}
	}
	submitted := invocations * len(lanes)
	if submitted < 1000 {
		t.Fatalf("precondition: the simulation issued only %d submissions; it must reproduce the incident's scale", submitted)
	}
	if got := countRequests(t, schedulerDir); got != len(lanes) {
		t.Errorf("%d outstanding requests after %d submissions over 59 hours, want %d", got, submitted, len(lanes))
	}
}

// A different source run is different work and must NOT be collapsed —
// otherwise idempotency would silently drop real re-ticks.
func TestPriorityTriggerKeysSeparateDistinctWork(t *testing.T) {
	schedulerDir := triggerQueueDir(t)
	for _, source := range []string{"run-1", "run-2", "run-3"} {
		if _, err := writePriorityTriggerRequest(schedulerDir, "g", "w", source); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRequests(t, schedulerDir); got != 3 {
		t.Errorf("%d outstanding requests for 3 distinct source runs, want 3", got)
	}
}

// Delegated (response-awaiting) requests must keep unique ids: two callers
// collapsed onto one id would race for a single response file, and one would
// consume the other's answer.
func TestDelegatedRequestsKeepDistinctIDs(t *testing.T) {
	schedulerDir := triggerQueueDir(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		id, err := writeTriggerRequestPayload(schedulerDir, triggerRequest{
			Workflow: "merge-review", Gaggle: "goobers", CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("delegated request %d reused id %q", i, id)
		}
		seen[id] = true
	}
	if got := countRequests(t, schedulerDir); got != 5 {
		t.Errorf("%d outstanding delegated requests, want 5", got)
	}
}

// The total safety cap catches the flood the per-identity bound cannot see:
// one spread across many DISTINCT identities.
func TestTotalSafetyCapRefusesADistinctIdentityFlood(t *testing.T) {
	schedulerDir := triggerQueueDir(t)
	restore := maxPendingTriggerRequests
	maxPendingTriggerRequests = 8
	t.Cleanup(func() { maxPendingTriggerRequests = restore })

	var lastErr error
	for i := 0; i < 50; i++ {
		_, err := writePriorityTriggerRequest(schedulerDir, "g", "workflow", "source-"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		if err != nil {
			lastErr = err
			break
		}
	}
	var full *errTriggerQueueFull
	if !errors.As(lastErr, &full) {
		t.Fatalf("error = %v, want the named queue-full refusal", lastErr)
	}
	for _, want := range []string{"safety cap", "#4326", "idempotency key"} {
		if !strings.Contains(lastErr.Error(), want) {
			t.Errorf("refusal %q does not mention %q", lastErr, want)
		}
	}
	if got := countRequests(t, schedulerDir); got > maxPendingTriggerRequests {
		t.Errorf("%d outstanding requests, want no more than the cap of %d", got, maxPendingTriggerRequests)
	}
}

// At the cap, re-submitting an ALREADY-outstanding keyed request must still
// succeed: it adds nothing to the queue, and refusing it would break
// idempotency exactly under the backlog it exists to prevent.
func TestKeyedResubmissionSucceedsAtTheCap(t *testing.T) {
	schedulerDir := triggerQueueDir(t)
	restore := maxPendingTriggerRequests
	maxPendingTriggerRequests = 3
	t.Cleanup(func() { maxPendingTriggerRequests = restore })

	var ids []string
	for i := 0; i < maxPendingTriggerRequests; i++ {
		id, err := writePriorityTriggerRequest(schedulerDir, "g", "w", "source-"+string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if got := countRequests(t, schedulerDir); got != maxPendingTriggerRequests {
		t.Fatalf("precondition: %d outstanding, want the queue exactly at the cap", got)
	}
	again, err := writePriorityTriggerRequest(schedulerDir, "g", "w", "source-a")
	if err != nil {
		t.Fatalf("re-submitting an outstanding keyed request at the cap was refused: %v", err)
	}
	if again != ids[0] {
		t.Errorf("re-submission returned %q, want the outstanding %q", again, ids[0])
	}
	// A NEW identity at the cap is still refused; only the no-op is exempt.
	if _, err := writePriorityTriggerRequest(schedulerDir, "g", "w", "source-new"); err == nil {
		t.Error("a new identity was admitted at the cap")
	}
}

// #4326 AC: "Record submissions and suppression decisions so a producer loop
// is visible before thousands of requests accumulate." The sweep's existing
// per-identity bounded-out journaling only fires once ONE identity exceeds
// five, so a flood spread across distinct identities was completely silent.
func TestSweepJournalsQueueDepthForADistinctIdentityFlood(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })

	restoreWarn := pendingTriggerWarnThreshold
	pendingTriggerWarnThreshold = 4
	t.Cleanup(func() { pendingTriggerWarnThreshold = restoreWarn })

	// Every request has a DIFFERENT identity, so the per-identity bound never
	// trips — this is precisely the blind spot the depth report closes.
	reqDir := filepath.Join(l.SchedulerDir(), pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := writePriorityTriggerRequest(l.SchedulerDir(), "g", "workflow", "source-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}

	reportPendingTriggerDepth(instanceLog, countRequests(t, l.SchedulerDir()))

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatalf("read instance log: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Error != nil && ev.Error.Code == "trigger_queue_deep" {
			found = true
			if !strings.Contains(ev.Error.Message, "6 pending trigger requests") {
				t.Errorf("event message %q does not carry the observed depth", ev.Error.Message)
			}
		}
	}
	if !found {
		t.Fatalf("no trigger_queue_deep event journalled; a distinct-identity flood stays invisible")
	}
}

// Below the threshold the sweep must stay quiet, or the signal is noise and
// an operator learns to ignore it.
func TestSweepDoesNotJournalAShallowQueue(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })

	reportPendingTriggerDepth(instanceLog, pendingTriggerWarnThreshold-1)

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Error != nil && strings.HasPrefix(ev.Error.Code, "trigger_queue_") {
			t.Fatalf("a shallow queue journalled %s", ev.Error.Code)
		}
	}
}
