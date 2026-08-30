package engine

import (
	"context"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
)

// memoedDescriber lists open workflows WITH the memo fields the daemon filters
// and resolves on. fakeDescriber's executions carry no memo at all, which is
// exactly the shape OpenRuns must reject.
type memoedDescriber struct {
	fakeDescriber
	memos map[string]map[string]string
}

func (f *memoedDescriber) ListWorkflow(_ context.Context, _ *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listPages > 0 {
		// Simulate an enumeration that never terminates within the page cap,
		// matching fakeDescriber's own unbounded shape.
		return &workflowservice.ListWorkflowExecutionsResponse{NextPageToken: []byte("more")}, nil
	}
	resp := &workflowservice.ListWorkflowExecutionsResponse{}
	for _, id := range f.open {
		info := &workflowpb.WorkflowExecutionInfo{
			Execution: &commonpb.WorkflowExecution{WorkflowId: id},
		}
		if fields := f.memos[id]; len(fields) > 0 {
			info.Memo = &commonpb.Memo{Fields: map[string]*commonpb.Payload{}}
			for k, v := range fields {
				payload, err := converter.GetDefaultDataConverter().ToPayload(v)
				if err != nil {
					return nil, err
				}
				info.Memo.Fields[k] = payload
			}
		}
		resp.Executions = append(resp.Executions, info)
	}
	return resp, nil
}

func webMemo(workflow string) map[string]string {
	return map[string]string{RunGaggleMemoKey: "web", RunWorkflowMemoKey: workflow}
}

func openRunsFor(t *testing.T, d *memoedDescriber, gaggles ...string) map[string]OpenRun {
	t.Helper()
	owned := make(map[string]struct{}, len(gaggles))
	for _, g := range gaggles {
		owned[g] = struct{}{}
	}
	open, err := NewWorkflowLiveness(d, "default").OpenRuns(context.Background(), owned)
	if err != nil {
		t.Fatalf("OpenRuns: %v", err)
	}
	return open
}

// TestOpenRunsNamesTheWorkflowBehindEveryOwnedRun is the D2 seam D1
// explicitly needs: scanOpenWorkflows' INVERSE.
//
// The DS6 liveness probe only needs to know THAT a run id maps to some open
// workflow, so it discards the workflow id and keeps a set. The daemon's boot
// reattach needs the opposite — given that an engine run was admitted, find
// and WAIT ON the workflow — and for a scheduled run the workflow id is not
// the run id (RunScheduled rewrites RunID to RunID(claimID)). Without the
// mapping the daemon describes the run id, gets NotFound, treats NotFound as
// settled, and releases the scheduler's concurrency slot underneath a live
// workflow — inviting a second, duplicate driver for the same workflow.
func TestOpenRunsNamesTheWorkflowBehindEveryOwnedRun(t *testing.T) {
	const (
		directID    = "goobers-web-implementation-direct"
		claimID     = "goobers-web-implementation-2026-01-02T00:00:00Z"
		scheduledID = claimID + scheduledRunWorkflowIDSuffix
	)
	describer := &memoedDescriber{
		fakeDescriber: fakeDescriber{open: []string{directID, scheduledID}},
		memos: map[string]map[string]string{
			directID:    webMemo("implementation"),
			scheduledID: webMemo("implementation"),
		},
	}
	open := openRunsFor(t, describer, "web")

	// A direct run's workflow id IS its run id (TemporalStarter.Start).
	direct, ok := open[directID]
	if !ok {
		t.Fatalf("direct run %q missing from %v", directID, open)
	}
	if direct.WorkflowID != directID || direct.RunID != directID {
		t.Errorf("direct run = %+v, want RunID and WorkflowID both %q", direct, directID)
	}

	// A scheduled run's journal is written under RunID(claimID), and the
	// workflow that must be waited on is the "-run" child. This mapping is
	// the entire point of the type.
	scheduled, ok := open[RunID(claimID)]
	if !ok {
		t.Fatalf("scheduled run %q missing from %v; the daemon would describe a run id the engine has never heard of", RunID(claimID), open)
	}
	if scheduled.WorkflowID != scheduledID {
		t.Errorf("scheduled run WorkflowID = %q, want the child workflow %q", scheduled.WorkflowID, scheduledID)
	}
	if scheduled.Gaggle != "web" || scheduled.Workflow != "implementation" {
		t.Errorf("scheduled run = %+v, want the memo-decoded gaggle and workflow", scheduled)
	}

	// And nothing else. A phantom key names a run directory that does not
	// exist, which reportOrphanedEngineRuns would surface as an orphan.
	if len(open) != 2 {
		t.Errorf("OpenRuns returned %d entries (%v), want exactly the two real runs", len(open), open)
	}
}

// TestOpenRunsPrefersTheChildOverItsScheduleClaim: while a scheduled run
// executes, BOTH the claim workflow and its child are open, and both derive
// the same run id. Only the child is the workflow the run is actually
// executing as — waiting on or describing the claim is not equivalent — and
// visibility returns executions in no defined order, so the choice cannot be
// left to whichever the scan happened to see last.
func TestOpenRunsPrefersTheChildOverItsScheduleClaim(t *testing.T) {
	const claimID = "goobers-web-implementation-2026-01-02T00:00:00Z"
	childID := claimID + scheduledRunWorkflowIDSuffix

	for _, order := range [][]string{{claimID, childID}, {childID, claimID}} {
		describer := &memoedDescriber{
			fakeDescriber: fakeDescriber{open: order},
			memos: map[string]map[string]string{
				claimID: webMemo("implementation"),
				childID: webMemo("implementation"),
			},
		}
		open := openRunsFor(t, describer, "web")
		run, ok := open[RunID(claimID)]
		if !ok {
			t.Fatalf("scan order %v: run %q missing", order, RunID(claimID))
		}
		if run.WorkflowID != childID {
			t.Errorf("scan order %v: WorkflowID = %q, want the executing child %q", order, run.WorkflowID, childID)
		}
	}

	// A claim whose child has not started yet is still the best answer
	// available, and must not be dropped.
	describer := &memoedDescriber{
		fakeDescriber: fakeDescriber{open: []string{claimID}},
		memos:         map[string]map[string]string{claimID: webMemo("implementation")},
	}
	open := openRunsFor(t, describer, "web")
	if run, ok := open[RunID(claimID)]; !ok || run.WorkflowID != claimID {
		t.Errorf("claim-only scan = %v, want %q mapped to the claim workflow", open, RunID(claimID))
	}
}

// TestOpenRunsFiltersByGaggleFailClosed: several instances may share one
// Temporal namespace, and reattaching to a sibling's run means two daemons
// driving one run's bookkeeping. The filter is therefore fail-closed — an
// empty owned-gaggle set matches NOTHING rather than everything — and a
// workflow with no gaggle memo is never claimed, because a filter must not
// admit a value it could not read.
func TestOpenRunsFiltersByGaggleFailClosed(t *testing.T) {
	describer := &memoedDescriber{
		fakeDescriber: fakeDescriber{open: []string{"ours", "theirs", "unlabelled"}},
		memos: map[string]map[string]string{
			"ours":   {RunGaggleMemoKey: "web"},
			"theirs": {RunGaggleMemoKey: "other-instance"},
		},
	}

	if empty := openRunsFor(t, describer); len(empty) != 0 {
		t.Fatalf("an empty owned-gaggle set matched %d runs, want none; a daemon with no gaggles must not reattach to a sibling's runs", len(empty))
	}

	owned := openRunsFor(t, describer, "web")
	if _, ok := owned["ours"]; !ok {
		t.Errorf("our own open run is missing from %v", owned)
	}
	if _, ok := owned["theirs"]; ok {
		t.Error("a sibling instance's open run was claimed")
	}
	if _, ok := owned["unlabelled"]; ok {
		t.Error("a workflow with no gaggle memo was claimed")
	}
}

// TestOpenRunsPropagatesEnumerationFailures: a scan that could not enumerate
// must report the failure rather than returning a partial map. A partial map
// read as authoritative is worse than no map at all — it says "this run has
// no open workflow" about a run whose page simply never arrived, which is
// precisely the NotFound-is-settled release the resolver exists to prevent.
func TestOpenRunsPropagatesEnumerationFailures(t *testing.T) {
	describer := &memoedDescriber{fakeDescriber: fakeDescriber{listErr: context.DeadlineExceeded}}
	if _, err := NewWorkflowLiveness(describer, "default").
		OpenRuns(context.Background(), map[string]struct{}{"web": {}}); err == nil {
		t.Fatal("OpenRuns swallowed an enumeration failure and returned a map the daemon would treat as authoritative")
	}
}

// TestLooksLikeScheduleClaimID guards the one heuristic in the derivation. It
// decides between two readings of a suffix-less workflow id that produce
// COMPLETELY different run ids, so a misread is a missed reattach.
func TestLooksLikeScheduleClaimID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"goobers-web-implementation-2026-01-02T00:00:00Z", true},
		{"goobers-web-implementation-2026-01-02T00:00:00.5Z", true},
		{"goobers-web-implementation-2026-01-02T00:00:00+01:00", true},
		{"goobers-web-implementation-direct", false},
		{"goobers-web-2026-01-02", false},
		{"", false},
		{"-", false},
	} {
		if got := looksLikeScheduleClaimID(tc.id); got != tc.want {
			t.Errorf("looksLikeScheduleClaimID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
