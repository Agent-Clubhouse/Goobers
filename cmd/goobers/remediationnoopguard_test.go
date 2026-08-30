package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// seedRemediationNoopState records one no-op attempt for key directly, standing
// in for a prior run's write. It uses the LOCK-FREE file store deliberately:
// the pre-#3989 updateRemediationNoopState took no lock of its own either (its
// callers held claims.lock), so a seeded fixture behaves identically and a test
// that deliberately holds claims.lock can still inspect the state.
func seedRemediationNoopState(
	t *testing.T,
	l instance.Layout,
	key string,
	signature remediationNoopSignature,
	runID string,
) {
	t.Helper()
	store, err := heldStateStore(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateRemediationNoopState(stateContext(), store, key, signature, runID); err != nil {
		t.Fatal(err)
	}
}

// remediationNoopStateRecord reads key's record without taking claims.lock.
func remediationNoopStateRecord(t *testing.T, l instance.Layout, key string) remediationNoopRecord {
	t.Helper()
	store, err := heldStateStore(l)
	if err != nil {
		t.Fatal(err)
	}
	record, err := readRemediationNoopRecord(stateContext(), store, key)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// clearSeededRemediationNoopState forgets key's record without taking
// claims.lock.
func clearSeededRemediationNoopState(t *testing.T, l instance.Layout, key string) {
	t.Helper()
	store, err := heldStateStore(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := clearRemediationNoopState(stateContext(), store, key); err != nil {
		t.Fatal(err)
	}
}

func TestRecordPRRemediationNoopCountsDistinctRuns(t *testing.T) {
	root := initDemo(t)
	l := layoutFor(root)
	const runID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "custom-remediation", Gaggle: "goobers",
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
	if _, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := recordPRRemediationNoop(l, runID); err != nil {
		t.Fatal(err)
	}
	if err := recordPRRemediationNoop(l, runID); err != nil {
		t.Fatal(err)
	}
	record := remediationNoopStateRecord(t, l, remediationNoopKey("", 77))
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
	seedRemediationNoopState(t, l, key, signature, "prior-run")
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
	if _, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := recordPRRemediationNoop(l, runID); err != nil {
		t.Fatal(err)
	}
	if record := remediationNoopStateRecord(t, l, key); !record.empty() {
		t.Fatalf("record = %+v, want successful implementation attempt to clear guard", record)
	}
}

func TestTerminalPRRemediationNoopLockTimeoutDefersRecordingToRecovery(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunConditions.ClaimsLockTimeout = "20ms"
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	const runID = "cccccccccccccccccccccccccccccccc"
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{
			Type: journal.EventStageFinished, Stage: "rebase-pr", Status: string(apiv1.ResultSuccess),
			Outputs: map[string]any{"attemptedHeadSha": "head-a", "remediationCauses": "substantive"},
		},
		{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultNoWork)},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	} {
		if err := jr.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatal(err)
	}

	manager, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	holder, err := lock.TryAcquire(filepath.Join(l.SchedulerDir(), claimLockFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeTerminalRun(l, nil, manager, runID); err != nil {
		t.Fatalf("terminal timeout should defer cleanup: %v", err)
	}
	if record := remediationNoopStateRecord(t, l, remediationNoopKey("", 77)); !record.empty() {
		t.Fatalf("record while claims lock held = %+v, want deferred update", record)
	}
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	released, err := recoverClaims(l, log, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].RunID != runID {
		t.Fatalf("recovery released %+v, want terminal remediation claim", released)
	}
	record := remediationNoopStateRecord(t, l, remediationNoopKey("", 77))
	if record.Attempts != 1 || record.LastRunID != runID {
		t.Fatalf("recovered no-op record = %+v, want one attempt for %s", record, runID)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := ledger.Lookup(pullRequestClaimKey(77)); held {
		t.Fatalf("terminal claim survived recovery: %+v", entry)
	}
}

func TestRemediationNoopAttemptsResetsOnHeadOrCauseChange(t *testing.T) {
	l := layoutFor(initDemo(t))
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	key := remediationNoopKey("goobers", 77)
	first := remediationNoopSignature{HeadSHA: "head-a", Causes: "substantive"}
	seedRemediationNoopState(t, l, key, first, "run-1")
	seedRemediationNoopState(t, l, key, first, "run-2")
	if record, err := remediationNoopRecordForSignature(l, 77, first); err != nil || record.Attempts != 2 {
		t.Fatalf("unchanged record = %+v, err = %v; want 2 attempts", record, err)
	}

	changedHead := remediationNoopSignature{HeadSHA: "head-b", Causes: "substantive"}
	if record, err := remediationNoopRecordForSignature(l, 77, changedHead); err != nil || record.Attempts != 0 {
		t.Fatalf("changed-head record = %+v, err = %v; want reset", record, err)
	}
	seedRemediationNoopState(t, l, key, changedHead, "run-3")
	changedCause := remediationNoopSignature{HeadSHA: "head-b", Causes: "failing-ci"}
	if record, err := remediationNoopRecordForSignature(l, 77, changedCause); err != nil || record.Attempts != 0 {
		t.Fatalf("changed-cause record = %+v, err = %v; want reset", record, err)
	}
	seedRemediationNoopState(t, l, key, changedCause, "run-4")
	clearSeededRemediationNoopState(t, l, key)
	if record, err := remediationNoopRecordForSignature(l, 77, changedCause); err != nil || record.Attempts != 0 {
		t.Fatalf("post-progress record = %+v, err = %v; want reset", record, err)
	}
}

func TestGatherPRContextDigestNoopIsIdempotentAndHonorsOperatorReset(t *testing.T) {
	l := layoutFor(initDemo(t))
	signature := remediationNoopSignature{HeadSHA: "head-a", DiffDigest: "sha256:diff-a"}
	key := remediationNoopKey("", 77)

	record, reset, err := recordGatherPRContextDigestNoop(l, 77, signature, "run-1", false)
	if err != nil || reset || record.Attempts != 1 {
		t.Fatalf("first record = %+v, reset = %v, err = %v; want one attempt", record, reset, err)
	}
	record, reset, err = recordGatherPRContextDigestNoop(l, 77, signature, "run-1", false)
	if err != nil || reset || record.Attempts != 1 {
		t.Fatalf("duplicate record = %+v, reset = %v, err = %v; want idempotent attempt", record, reset, err)
	}
	record, reset, err = recordGatherPRContextDigestNoop(l, 77, signature, "run-2", false)
	if err != nil || reset || record.Attempts != remediationNoopLimit {
		t.Fatalf("second run record = %+v, reset = %v, err = %v; want limit %d", record, reset, err, remediationNoopLimit)
	}
	if err := markRemediationNoopParked(l, key); err != nil {
		t.Fatal(err)
	}

	record, reset, err = recordGatherPRContextDigestNoop(l, 77, signature, "run-3", false)
	if err != nil || !reset || record.Attempts != 0 {
		t.Fatalf("operator reset record = %+v, reset = %v, err = %v; want cleared guard", record, reset, err)
	}
	if record := remediationNoopStateRecord(t, l, key); !record.empty() {
		t.Fatalf("record = %+v, want operator-cleared guard removed", record)
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
	seedRemediationNoopState(t, layoutFor(root), key, signature, "run-1")
	seedRemediationNoopState(t, layoutFor(root), key, signature, "run-2")

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

func TestRemediationCheckpointResetsNoopGuardWhenHeadChangesBeforePublication(t *testing.T) {
	const branch = "goobers/impl/remediation-364"
	baseSHA, initialHeadSHA := initRemediationCheckpointRepo(t, branch)
	runGitT(t, ".", "checkout", "-B", branch, "origin/"+branch)

	origin := strings.TrimSpace(runGitOutputT(t, ".", "remote", "get-url", "origin"))
	concurrent := filepath.Join(t.TempDir(), "concurrent")
	runGitT(t, ".", "clone", "--branch", branch, origin, concurrent)
	runGitT(t, concurrent, "config", "user.name", "human")
	runGitT(t, concurrent, "config", "user.email", "human@example.com")
	if err := os.WriteFile(filepath.Join(concurrent, "concurrent.txt"), []byte("new head\n"), 0o644); err != nil {
		t.Fatalf("write concurrent change: %v", err)
	}
	runGitT(t, concurrent, "add", "concurrent.txt")
	runGitT(t, concurrent, "commit", "-m", "concurrent PR update")
	runGitT(t, concurrent, "push", "origin", "HEAD")
	advancedHeadSHA := strings.TrimSpace(runGitOutputT(t, concurrent, "rev-parse", "HEAD"))

	st := &remediationCheckpointServerState{
		number: 77, headSHA: initialHeadSHA, baseSHA: baseSHA,
		labels:            []string{needsRemediationLabel},
		headAfterComments: advancedHeadSHA,
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	root := remediationCheckpointEnv(t, server.URL, false)
	key := remediationNoopKey("", 77)
	signature := remediationNoopSignature{HeadSHA: initialHeadSHA, Causes: "substantive"}
	seedRemediationNoopState(t, layoutFor(root), key, signature, "run-1")
	seedRemediationNoopState(t, layoutFor(root), key, signature, "run-2")

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) {
		t.Fatalf("labels = %v, concurrent head must reset the prior head's no-op guard", st.labels)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.Escalated || state.HeadSHA != advancedHeadSHA {
		t.Fatalf("checkpoint state = %+v, ok = %v, want ordinary checkpoint for advanced head %s", state, ok, advancedHeadSHA)
	}
	if record := remediationNoopStateRecord(t, layoutFor(root), key); !record.empty() {
		t.Fatalf("no-op record = %+v, want stale head attempts cleared", record)
	}
}
