package engine

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/api/serviceerror"
)

// resolveworkflowid_test.go covers decision 005 D2 (#3877): the inverse of the
// open-workflow scan, read by the daemon's run guards when a describe on a
// run's OWN id returns NotFound.
//
// The gap it closes is decision 003's engine_run_unresolvable case. A
// scheduled run's RunID is a hash of its Schedule claim workflow's id
// (RunScheduled), so `guards.await`/`guards.cancel` describing the run id get
// NotFound for a run that is executing perfectly well — and NotFound is
// treated as SETTLED, which releases the scheduler's concurrency slot
// underneath a live workflow and invites a second driver for the same
// workflow.

const (
	testClaimID  = "goobers-aaaa-2026-08-30T01:00:00Z"
	testDirectID = "goobers-web-implementation-direct"
)

func ownedSet(gaggles ...string) map[string]struct{} {
	owned := make(map[string]struct{}, len(gaggles))
	for _, g := range gaggles {
		owned[g] = struct{}{}
	}
	return owned
}

// TestResolveWorkflowIDNamesTheWorkflowBehindARewrittenRunID is the D2
// headline: a run id the Schedule mechanism REWROTE resolves back to the
// workflow that is actually executing it — the "-run" child, never the claim,
// because only the child is the execution a caller can wait on or cancel.
func TestResolveWorkflowIDNamesTheWorkflowBehindARewrittenRunID(t *testing.T) {
	childID := scheduledRunWorkflowID(testClaimID)
	describer := &memoedDescriber{
		fakeDescriber: fakeDescriber{open: []string{testClaimID, childID}},
		memos: map[string]map[string]string{
			testClaimID: webMemo("implementation"),
			childID:     webMemo("implementation"),
		},
	}
	probe := NewWorkflowLiveness(describer, "default")

	workflowID, err := probe.ResolveWorkflowID(context.Background(), RunID(testClaimID), ownedSet("web"))
	if err != nil {
		t.Fatalf("ResolveWorkflowID: %v — a scheduled run the daemon cannot name is one it releases the slot for", err)
	}
	if workflowID != childID {
		t.Fatalf("resolved workflow id = %q, want the executing child %q", workflowID, childID)
	}
}

// TestResolveWorkflowIDPrefersTheChildInEitherScanOrder: visibility returns
// executions in no defined order, so the claim-versus-child choice cannot be
// left to whichever the scan happened to see last. A claim with no child yet
// is still the best (and only) answer available, and must not be dropped.
func TestResolveWorkflowIDPrefersTheChildInEitherScanOrder(t *testing.T) {
	childID := scheduledRunWorkflowID(testClaimID)
	memos := map[string]map[string]string{
		testClaimID: webMemo("implementation"),
		childID:     webMemo("implementation"),
	}
	for _, order := range [][]string{{testClaimID, childID}, {childID, testClaimID}} {
		describer := &memoedDescriber{fakeDescriber: fakeDescriber{open: order}, memos: memos}
		got, err := NewWorkflowLiveness(describer, "default").
			ResolveWorkflowID(context.Background(), RunID(testClaimID), ownedSet("web"))
		if err != nil || got != childID {
			t.Errorf("scan order %v: resolved %q (err %v), want the child %q", order, got, err, childID)
		}
	}

	claimOnly := &memoedDescriber{
		fakeDescriber: fakeDescriber{open: []string{testClaimID}},
		memos:         map[string]map[string]string{testClaimID: webMemo("implementation")},
	}
	got, err := NewWorkflowLiveness(claimOnly, "default").
		ResolveWorkflowID(context.Background(), RunID(testClaimID), ownedSet("web"))
	if err != nil || got != testClaimID {
		t.Errorf("claim-only scan: resolved %q (err %v), want the claim workflow %q", got, err, testClaimID)
	}
}

// TestResolveWorkflowIDDistinguishesNotOpenFromUnknown is the distinction the
// whole guard rests on. ErrRunNotOpen is DEFINITE — nothing on the engine is
// driving this run, so settling it is correct. An enumeration that could not
// complete (visibility down, or the page cap exceeded) is UNKNOWN, and a
// caller that treats unknown as "not open" releases a live run's slot.
func TestResolveWorkflowIDDistinguishesNotOpenFromUnknown(t *testing.T) {
	absent := &memoedDescriber{
		fakeDescriber: fakeDescriber{open: []string{"someone-elses-workflow"}},
		memos:         map[string]map[string]string{"someone-elses-workflow": webMemo("implementation")},
	}
	if _, err := NewWorkflowLiveness(absent, "default").
		ResolveWorkflowID(context.Background(), RunID(testClaimID), ownedSet("web")); !errors.Is(err, ErrRunNotOpen) {
		t.Fatalf("absent run: err = %v, want ErrRunNotOpen (a definite answer)", err)
	}

	broken := &memoedDescriber{fakeDescriber: fakeDescriber{listErr: errors.New("visibility store unavailable")}}
	_, err := NewWorkflowLiveness(broken, "default").
		ResolveWorkflowID(context.Background(), RunID(testClaimID), ownedSet("web"))
	if err == nil || errors.Is(err, ErrRunNotOpen) {
		t.Fatalf("failed enumeration: err = %v, want an unknown error that is NOT ErrRunNotOpen", err)
	}
}

// TestResolveWorkflowIDReportsThePageCapAsUnknown: the scan cache TTL and page
// cap bound how many concurrent runs the scheduled-run inverse can resolve
// (finding 002's "DS6 liveness open-workflow scan budget" risk). Past the cap
// the mapping is UNKNOWN — an error — never a short answer, because a
// truncated enumeration says "this run has no open workflow" about a run whose
// page simply never arrived.
func TestResolveWorkflowIDReportsThePageCapAsUnknown(t *testing.T) {
	unbounded := &memoedDescriber{fakeDescriber: fakeDescriber{listPages: 1}}
	_, err := NewWorkflowLiveness(unbounded, "default").
		ResolveWorkflowID(context.Background(), RunID(testClaimID), ownedSet("web"))
	if err == nil || errors.Is(err, ErrRunNotOpen) {
		t.Fatalf("page cap exceeded: err = %v, want an unknown error that is NOT ErrRunNotOpen", err)
	}
	if unbounded.listCalls != maxOpenWorkflowScanPages {
		t.Errorf("list calls = %d, want the page cap %d — the scan must stop, not page forever",
			unbounded.listCalls, maxOpenWorkflowScanPages)
	}
}

// TestResolveWorkflowIDIsLoudAboutAmbiguity: two UNRELATED open workflows
// claiming one run id is not something to resolve by coin flip — the loser is
// a different run (or a different gaggle's run), and the caller is about to
// wait on or CANCEL whichever is picked. It must be reported.
func TestResolveWorkflowIDIsLoudAboutAmbiguity(t *testing.T) {
	// A direct run whose workflow id collides with a scheduled run's rewritten
	// run id: two genuinely different executions, one key.
	collidingRunID := RunID(testClaimID)
	childID := scheduledRunWorkflowID(testClaimID)
	describer := &memoedDescriber{
		fakeDescriber: fakeDescriber{open: []string{collidingRunID, childID}},
		memos: map[string]map[string]string{
			collidingRunID: webMemo("implementation"),
			childID:        webMemo("implementation"),
		},
	}
	probe := NewWorkflowLiveness(describer, "default")
	if _, err := probe.ResolveWorkflowID(context.Background(), collidingRunID, ownedSet("web")); !errors.Is(err, ErrAmbiguousRunID) {
		t.Fatalf("colliding run id: err = %v, want ErrAmbiguousRunID", err)
	}

	// And the boot enumeration omits it rather than guessing: a guessed entry
	// names the wrong workflow for a real run directory.
	open, err := probe.OpenRuns(context.Background(), ownedSet("web"))
	if err != nil {
		t.Fatalf("OpenRuns: %v", err)
	}
	if run, ok := open[collidingRunID]; ok {
		t.Fatalf("OpenRuns guessed %+v for an ambiguous run id; it must omit it", run)
	}
}

// TestResolveWorkflowIDIsGaggleContained: several instances may share one
// Temporal namespace. A run id resolvable only through a SIBLING's workflow
// must answer ErrRunNotOpen, not hand this daemon a workflow to cancel — the
// scan is shared, the ownership is not.
func TestResolveWorkflowIDIsGaggleContained(t *testing.T) {
	childID := scheduledRunWorkflowID(testClaimID)
	describer := &memoedDescriber{
		fakeDescriber: fakeDescriber{open: []string{childID}},
		memos:         map[string]map[string]string{childID: {RunGaggleMemoKey: "other-instance"}},
	}
	probe := NewWorkflowLiveness(describer, "default")
	if _, err := probe.ResolveWorkflowID(context.Background(), RunID(testClaimID), ownedSet("web")); !errors.Is(err, ErrRunNotOpen) {
		t.Fatalf("sibling's workflow: err = %v, want ErrRunNotOpen", err)
	}
	// Fail-closed: a caller that has not said which gaggles it owns resolves
	// nothing at all, and does not even scan.
	if _, err := probe.ResolveWorkflowID(context.Background(), RunID(testClaimID), nil); !errors.Is(err, ErrRunNotOpen) {
		t.Fatalf("empty owned-gaggle set: err = %v, want ErrRunNotOpen", err)
	}
}

// TestOpenWorkflowScanServesEveryConsumerOnce is D2's "one scan serving two
// callers" (three, with the guards). The DS6 liveness probe, the daemon's boot
// reattach enumeration and a guard's NotFound resolution all read the same
// TTL-cached index, so a namespace is paged once per TTL however many callers
// ask — the scan-budget bound finding 002 flagged.
func TestOpenWorkflowScanServesEveryConsumerOnce(t *testing.T) {
	ctx := context.Background()
	childID := scheduledRunWorkflowID(testClaimID)
	describer := &memoedDescriber{
		fakeDescriber: fakeDescriber{
			err:  serviceerror.NewNotFound("workflow not found"),
			open: []string{childID},
		},
		memos: map[string]map[string]string{childID: webMemo("implementation")},
	}
	probe := NewWorkflowLiveness(describer, "default")

	if live, err := probe.RunLive(ctx, RunID(testClaimID)); err != nil || !live {
		t.Fatalf("RunLive: live=%v err=%v, want live", live, err)
	}
	if _, err := probe.OpenRuns(ctx, ownedSet("web")); err != nil {
		t.Fatalf("OpenRuns: %v", err)
	}
	if _, err := probe.ResolveWorkflowID(ctx, RunID(testClaimID), ownedSet("web")); err != nil {
		t.Fatalf("ResolveWorkflowID: %v", err)
	}
	if describer.listCalls != 1 {
		t.Fatalf("open-workflow scans = %d, want 1 — three consumers, one cached enumeration", describer.listCalls)
	}
}

// TestOpenWorkflowScanRefreshesAfterItsTTL: the cache is a budget device, not
// a snapshot. Once it expires the next consumer rescans, so a run started
// after the boot enumeration is still resolvable — and a failed refresh does
// NOT silently serve the stale index, because a stale map read as
// authoritative names workflows that may have closed.
func TestOpenWorkflowScanRefreshesAfterItsTTL(t *testing.T) {
	ctx := context.Background()
	childID := scheduledRunWorkflowID(testClaimID)
	describer := &memoedDescriber{fakeDescriber: fakeDescriber{}}
	probe := NewWorkflowLiveness(describer, "default")

	// Nothing open yet: a definite "not open".
	if _, err := probe.ResolveWorkflowID(ctx, RunID(testClaimID), ownedSet("web")); !errors.Is(err, ErrRunNotOpen) {
		t.Fatalf("empty namespace: err = %v, want ErrRunNotOpen", err)
	}

	// The run starts. Within the TTL the cached answer still stands.
	describer.open = []string{childID}
	describer.memos = map[string]map[string]string{childID: webMemo("implementation")}
	if _, err := probe.ResolveWorkflowID(ctx, RunID(testClaimID), ownedSet("web")); !errors.Is(err, ErrRunNotOpen) {
		t.Fatalf("within TTL: err = %v, want the cached ErrRunNotOpen", err)
	}
	if describer.listCalls != 1 {
		t.Fatalf("list calls = %d within the TTL, want 1", describer.listCalls)
	}

	// Expire it exactly as the passage of time would.
	expireOpenWorkflowScan(probe)
	got, err := probe.ResolveWorkflowID(ctx, RunID(testClaimID), ownedSet("web"))
	if err != nil || got != childID {
		t.Fatalf("after TTL: resolved %q (err %v), want a rescan naming %q", got, err, childID)
	}
	if describer.listCalls != 2 {
		t.Fatalf("list calls = %d after the TTL, want a second scan", describer.listCalls)
	}

	// A refresh that FAILS is unknown, never the stale index.
	expireOpenWorkflowScan(probe)
	describer.listErr = errors.New("visibility store unavailable")
	if _, err := probe.ResolveWorkflowID(ctx, RunID(testClaimID), ownedSet("web")); err == nil || errors.Is(err, ErrRunNotOpen) {
		t.Fatalf("failed refresh: err = %v, want an unknown error rather than a stale answer", err)
	}
}

// TestResolveWorkflowIDLeavesTheDirectRunPathUntouched: the common case after
// D1 is WorkflowID == RunID, and it must stay free. A direct run resolves to
// itself, and nothing in the guards reaches here for one at all — RunLive
// proves that half by never scanning when its describe answers.
func TestResolveWorkflowIDLeavesTheDirectRunPathUntouched(t *testing.T) {
	describer := &memoedDescriber{
		fakeDescriber: fakeDescriber{open: []string{testDirectID}},
		memos:         map[string]map[string]string{testDirectID: webMemo("implementation")},
	}
	got, err := NewWorkflowLiveness(describer, "default").
		ResolveWorkflowID(context.Background(), testDirectID, ownedSet("web"))
	if err != nil || got != testDirectID {
		t.Fatalf("direct run: resolved %q (err %v), want its own id %q", got, err, testDirectID)
	}
}

// expireOpenWorkflowScan ages the cached scan past its TTL, standing in for
// the passage of openWorkflowScanCacheTTL without sleeping for it.
func expireOpenWorkflowScan(p *WorkflowLiveness) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scannedAt = p.scannedAt.Add(-2 * openWorkflowScanCacheTTL)
}
