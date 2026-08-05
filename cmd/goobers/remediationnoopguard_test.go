package main

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestRecordPRRemediationNoopCountsDistinctRuns(t *testing.T) {
	root := initDemo(t)
	l := layoutFor(root)
	const runID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "rebase-pr", Status: string(apiv1.ResultSuccess),
		Outputs: map[string]any{"attemptedHeadSha": "head-a", "remediationCauses": "substantive"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultNoWork),
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := claimPullRequestInOrder(root, []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := recordPRRemediationNoop(l, runID); err != nil {
		t.Fatal(err)
	}
	if err := recordPRRemediationNoop(l, runID); err != nil {
		t.Fatal(err)
	}
	state, err := readRemediationNoopState(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	record := state.Records[remediationNoopKey("", 77)]
	if record.Attempts != 1 || record.LastRunID != runID || record.HeadSHA != "head-a" || record.Causes != "substantive" {
		t.Fatalf("record = %+v, want one idempotently recorded no-op", record)
	}
}

func TestRecordPRRemediationNoopSuccessThenNoWorkClearsGuard(t *testing.T) {
	root := initDemo(t)
	l := layoutFor(root)
	const runID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	key := remediationNoopKey("", 77)
	signature := remediationNoopSignature{HeadSHA: "head-a", Causes: "substantive"}
	if err := updateRemediationNoopState(l.SchedulerDir(), key, signature, "prior-run"); err != nil {
		t.Fatal(err)
	}
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []apiv1.ResultStatus{apiv1.ResultSuccess, apiv1.ResultNoWork} {
		if err := jr.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: "implement", Status: string(status),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := claimPullRequestInOrder(root, []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := recordPRRemediationNoop(l, runID); err != nil {
		t.Fatal(err)
	}
	state, err := readRemediationNoopState(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Records[key]; ok {
		t.Fatalf("record = %+v, want successful implementation attempt to clear guard", state.Records[key])
	}
}

func TestRemediationNoopAttemptsResetsOnHeadOrCauseChange(t *testing.T) {
	l := layoutFor(initDemo(t))
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	key := remediationNoopKey("goobers", 77)
	first := remediationNoopSignature{HeadSHA: "head-a", Causes: "substantive"}
	if err := updateRemediationNoopState(l.SchedulerDir(), key, first, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := updateRemediationNoopState(l.SchedulerDir(), key, first, "run-2"); err != nil {
		t.Fatal(err)
	}
	if record, err := remediationNoopRecordForSignature(l, 77, first); err != nil || record.Attempts != 2 {
		t.Fatalf("unchanged record = %+v, err = %v; want 2 attempts", record, err)
	}

	changedHead := remediationNoopSignature{HeadSHA: "head-b", Causes: "substantive"}
	if record, err := remediationNoopRecordForSignature(l, 77, changedHead); err != nil || record.Attempts != 0 {
		t.Fatalf("changed-head record = %+v, err = %v; want reset", record, err)
	}
	if err := updateRemediationNoopState(l.SchedulerDir(), key, changedHead, "run-3"); err != nil {
		t.Fatal(err)
	}
	changedCause := remediationNoopSignature{HeadSHA: "head-b", Causes: "failing-ci"}
	if record, err := remediationNoopRecordForSignature(l, 77, changedCause); err != nil || record.Attempts != 0 {
		t.Fatalf("changed-cause record = %+v, err = %v; want reset", record, err)
	}
	if err := updateRemediationNoopState(l.SchedulerDir(), key, changedCause, "run-4"); err != nil {
		t.Fatal(err)
	}
	if err := clearRemediationNoopState(l.SchedulerDir(), key); err != nil {
		t.Fatal(err)
	}
	if record, err := remediationNoopRecordForSignature(l, 77, changedCause); err != nil || record.Attempts != 0 {
		t.Fatalf("post-progress record = %+v, err = %v; want reset", record, err)
	}
}

func TestRemediationCheckpointParksRepeatedNoopSignature(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{needsRemediationLabel},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	root := remediationCheckpointEnv(t, server.URL, false)
	signature := remediationNoopSignature{HeadSHA: headSHA, Causes: "substantive"}
	key := remediationNoopKey("", 77)
	if err := updateRemediationNoopState(layoutFor(root).SchedulerDir(), key, signature, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := updateRemediationNoopState(layoutFor(root).SchedulerDir(), key, signature, "run-2"); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	st.mu.Lock()
	if !hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) ||
		hasAnyLabel(st.labels, []string{needsRemediationLabel}) {
		st.mu.Unlock()
		t.Fatalf("labels = %v, want parked escalation", st.labels)
	}
	if len(st.comments) != 1 || !strings.Contains(st.comments[0], "reported no-work 2 consecutive times") {
		st.mu.Unlock()
		t.Fatalf("comments = %v, want visible no-progress reason", st.comments)
	}
	st.labels = []string{needsRemediationLabel}
	st.mu.Unlock()

	code, stdout, stderr = runArgs(t, "remediation-checkpoint", root)
	if code != 0 {
		t.Fatalf("operator-reset code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) {
		t.Fatalf("labels = %v after operator cleared escalation, want no-op guard reset", st.labels)
	}
}
