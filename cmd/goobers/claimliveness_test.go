package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// stubProbe answers live for exactly the run ids mapped true; every other
// run id is definitively not live. A test stand-in for the engine half of
// DS6's probe (an open Temporal workflow).
type stubProbe map[string]bool

func (p stubProbe) RunLive(_ context.Context, runID string) (bool, error) {
	return p[runID], nil
}

func newClaimTestLayout(t *testing.T) instance.Layout {
	t.Helper()
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return l
}

// TestRecoverClaimsIsNoOpWhileRecoveryGateClosed is DS6's load-bearing
// startup ordering (distributed-state-and-coordination.md §10), proven at the
// reap pass itself: until the renewal set has been rebuilt from ledger +
// liveness, RecoverExpired provably does not run — an expired lease survives
// the pass untouched — and the same pass reaps it once the gate is open.
func TestRecoverClaimsIsNoOpWhileRecoveryGateClosed(t *testing.T) {
	l := newClaimTestLayout(t)
	past := time.Now().Add(-2 * time.Hour)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(l.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return past }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("issue-9", "remote-run", "implementation", time.Minute); err != nil || !ok {
		t.Fatalf("seed expired claim: ok=%v err=%v", ok, err)
	}
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	gate := localscheduler.NewRecoveryGate()
	released, err := recoverClaims(l, log, time.Now(), nil, gate)
	if err != nil || len(released) != 0 {
		t.Fatalf("closed gate: released=%+v err=%v, want a no-op pass", released, err)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.Lookup("issue-9"); !held {
		t.Fatal("expired claim was reaped before the renewal set was rebuilt")
	}

	gate.MarkRenewalRebuilt()
	released, err = recoverClaims(l, log, time.Now(), nil, gate)
	if err != nil || len(released) != 1 || released[0].ItemID != "issue-9" {
		t.Fatalf("open gate: released=%+v err=%v, want the expired claim reaped", released, err)
	}
}

// TestRenewLiveClaimsLedgerDrivenAcrossRestart is DS6's renewal re-keying
// (distributed-state-and-coordination.md §10): the renewal candidates come
// from the LEDGER, and an untracked run — the restarted daemon lost its
// in-process tracking while the run keeps executing on the engine — is
// renewed when the liveness probe reports its workflow open, while a run
// whose workflow is closed or vanished is skipped so its lease lapses
// normally into RecoverExpired.
func TestRenewLiveClaimsLedgerDrivenAcrossRestart(t *testing.T) {
	l := newClaimTestLayout(t)
	// Both leases expire 10 minutes from now: the restart gap under test is
	// shorter than the lease, so neither claim is expired when the "restarted
	// daemon" rebuilds its renewal set.
	seed, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	for item, run := range map[string]string{"issue-10": "engine-live-run", "issue-11": "engine-closed-run"} {
		if ok, _, err := seed.Claim(item, run, "implementation", 10*time.Minute); err != nil || !ok {
			t.Fatalf("seed %s: ok=%v err=%v", item, ok, err)
		}
	}

	// The restarted daemon tracks NOTHING in-process; only the engine probe
	// knows engine-live-run's workflow is still open.
	probe := localscheduler.CompositeRunLiveness(
		localscheduler.TrackedRunLiveness(func() []string { return nil }),
		stubProbe{"engine-live-run": true},
	)
	renewed, probeErr, err := renewLiveClaims(context.Background(), l, probe, DefaultClaimLease)
	if probeErr != nil || err != nil {
		t.Fatalf("renewLiveClaims: probeErr=%v err=%v", probeErr, err)
	}
	if len(renewed) != 1 || renewed[0].ItemID != "issue-10" {
		t.Fatalf("renewed = %+v, want exactly the engine-live holder's claim", renewed)
	}

	// Past the original lease window — inside the renewed one. The live run's
	// claim survives recovery; the closed run's lease lapsed normally.
	after := time.Now().Add(11 * time.Minute)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	gate := localscheduler.NewRecoveryGate()
	gate.MarkRenewalRebuilt()
	released, err := recoverClaims(l, log, after, nil, gate)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].ItemID != "issue-11" {
		t.Fatalf("released = %+v, want only the closed workflow's lease to lapse", released)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := reopened.Lookup("issue-10"); !held || entry.RunID != "engine-live-run" {
		t.Fatalf("issue-10 = %+v held=%v, want still held by the live engine run", entry, held)
	}
}

// TestRenewLiveClaimsFailsLiveOnProbeError: an erroring probe (engine
// unreachable — liveness UNKNOWN) renews rather than letting the lease drift
// toward the reaper, and surfaces the degradation as probeErr while err stays
// nil (the ledger write completed, so a RecoveryGate may open).
func TestRenewLiveClaimsFailsLiveOnProbeError(t *testing.T) {
	l := newClaimTestLayout(t)
	seed, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := seed.Claim("issue-12", "unknown-run", "implementation", 10*time.Minute); err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
	failing := localscheduler.CompositeRunLiveness(erroringProbe{})
	renewed, probeErr, err := renewLiveClaims(context.Background(), l, failing, DefaultClaimLease)
	if err != nil {
		t.Fatalf("err = %v, want nil: the ledger half completed", err)
	}
	if probeErr == nil {
		t.Fatal("probeErr = nil, want the fail-live degradation surfaced")
	}
	if len(renewed) != 1 || renewed[0].ItemID != "issue-12" {
		t.Fatalf("renewed = %+v, want the unknown-liveness claim renewed fail-live", renewed)
	}
}

type erroringProbe struct{}

func (erroringProbe) RunLive(context.Context, string) (bool, error) {
	return false, errors.New("engine frontend unreachable")
}

// TestBuildClaimLivenessProbeMode1NeverDialsEngine is the frozen mode-1
// contract: with no `engine:` config the probe is exactly today's tracked
// in-process signal — no engine client is dialed and only tracked runs are
// live.
func TestBuildClaimLivenessProbeMode1NeverDialsEngine(t *testing.T) {
	prev := engineLivenessProbe
	engineLivenessProbe = func(*instance.Config) (localscheduler.RunLivenessProbe, func(), error) {
		t.Fatal("mode-1 instance must not build an engine liveness probe")
		return nil, nil, nil
	}
	t.Cleanup(func() { engineLivenessProbe = prev })

	probe, closeProbe, err := buildClaimLivenessProbe(&instance.Config{}, func() []string { return []string{"tracked-run"} })
	if err != nil {
		t.Fatal(err)
	}
	defer closeProbe()
	if live, err := probe.RunLive(context.Background(), "tracked-run"); err != nil || !live {
		t.Fatalf("tracked run: live=%v err=%v, want live", live, err)
	}
	if live, err := probe.RunLive(context.Background(), "untracked-run"); err != nil || live {
		t.Fatalf("untracked run: live=%v err=%v, want not live", live, err)
	}
}

// fakeTemporalLiveness backs engine.NewWorkflowLiveness in renewal-level
// tests: DescribeWorkflowExecution answers NotFound for every id (no workflow
// executes under a scheduled run's hashed RunID) and ListWorkflow reports the
// given workflow ids open.
type fakeTemporalLiveness struct{ open []string }

func (f *fakeTemporalLiveness) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return nil, serviceerror.NewNotFound("workflow not found")
}

func (f *fakeTemporalLiveness) ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	resp := &workflowservice.ListWorkflowExecutionsResponse{}
	for _, id := range f.open {
		resp.Executions = append(resp.Executions, &workflowpb.WorkflowExecutionInfo{
			Execution: &commonpb.WorkflowExecution{WorkflowId: id},
		})
	}
	return resp, nil
}

// TestRenewLiveClaimsCoversScheduledEngineRun (#3512 review, finding 1): a
// SCHEDULED engine run claims under RunID(claimID) — a hash — while its
// workflows execute under claimID (the Schedule claim) and claimID+"-run"
// (the child), so a describe of the recorded RunID is NotFound. Before the
// open-workflow mapping that read as DEFINITIVELY not live — no probe error,
// no fail-live — and renewal silently never covered scheduled runs. The
// renewal pass must renew such a claim while either workflow is open, and a
// genuinely closed scheduled workflow's lease must still lapse into recovery.
func TestRenewLiveClaimsCoversScheduledEngineRun(t *testing.T) {
	l := newClaimTestLayout(t)
	claimID := "goobers-instancehash-claimhash-2026-08-22T01:00:00Z"
	hashedRunID := engine.RunID(claimID)

	seed, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := seed.Claim("issue-20", hashedRunID, "scheduled-implement", 10*time.Minute); err != nil || !ok {
		t.Fatalf("seed scheduled-run claim: ok=%v err=%v", ok, err)
	}

	// The scheduled run's workflows are open; the daemon tracks nothing.
	openProbe := localscheduler.CompositeRunLiveness(
		localscheduler.TrackedRunLiveness(func() []string { return nil }),
		engine.NewWorkflowLiveness(&fakeTemporalLiveness{open: []string{claimID, claimID + "-run"}}, "default"),
	)
	renewed, probeErr, err := renewLiveClaims(context.Background(), l, openProbe, DefaultClaimLease)
	if probeErr != nil || err != nil {
		t.Fatalf("renewLiveClaims: probeErr=%v err=%v", probeErr, err)
	}
	if len(renewed) != 1 || renewed[0].ItemID != "issue-20" {
		t.Fatalf("renewed = %+v, want the scheduled run's claim renewed, not skipped as not-live", renewed)
	}

	// Once neither workflow is open the run is definitively not live: the
	// claim is not renewed and its lease lapses normally into recovery.
	closedProbe := localscheduler.CompositeRunLiveness(
		localscheduler.TrackedRunLiveness(func() []string { return nil }),
		engine.NewWorkflowLiveness(&fakeTemporalLiveness{}, "default"),
	)
	renewed, probeErr, err = renewLiveClaims(context.Background(), l, closedProbe, DefaultClaimLease)
	if probeErr != nil || err != nil {
		t.Fatalf("renewLiveClaims (closed): probeErr=%v err=%v", probeErr, err)
	}
	if len(renewed) != 0 {
		t.Fatalf("renewed = %+v, want nothing renewed for a closed scheduled workflow", renewed)
	}
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	gate := localscheduler.NewRecoveryGate()
	gate.MarkRenewalRebuilt()
	released, err := recoverClaims(l, log, time.Now().Add(DefaultClaimLease+time.Minute), nil, gate)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].ItemID != "issue-20" {
		t.Fatalf("released = %+v, want the closed scheduled workflow's lease to lapse", released)
	}
}

// TestUpDoesNotReapEngineLiveClaimAcrossRestart is DS6's daemon-level
// acceptance (distributed-state-and-coordination.md §13 item 5's local
// analog): a daemon restart finds a lease that EXPIRED while the daemon was
// down, held by a run it does not track — the distributed-run shape — whose
// workflow the engine reports open. The renewal set is rebuilt before any
// reap is permitted, so the claim is renewed, survives startup recovery, and
// keeps refusing a second claimant. Were RecoverExpired to run first (the
// pre-DS6 order), the expired lease would be reaped and the item handed to a
// second run.
func TestUpDoesNotReapEngineLiveClaimAcrossRestart(t *testing.T) {
	root := initDeterministicDemo(t)
	schedulerDir := filepath.Join(root, "scheduler")

	prevProbe := buildClaimLivenessProbe
	buildClaimLivenessProbe = func(cfg *instance.Config, tracked func() []string) (localscheduler.RunLivenessProbe, func(), error) {
		return localscheduler.CompositeRunLiveness(
			localscheduler.TrackedRunLiveness(tracked),
			stubProbe{"engine-live-run": true},
		), func() {}, nil
	}
	t.Cleanup(func() { buildClaimLivenessProbe = prevProbe })

	past := time.Now().Add(-2 * time.Hour)
	seedLedger, err := localscheduler.OpenClaimLedger(
		filepath.Join(schedulerDir, claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return past }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := seedLedger.Claim("issue-13", "engine-live-run", "implementation", time.Minute); err != nil || !ok {
		t.Fatalf("seed expired claim: ok=%v err=%v", ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{root}, &stdout, &stderr) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not return after ctx cancellation")
	}

	if strings.Contains(stdout.String(), "recovered expired claim issue-13") {
		t.Fatalf("stdout = %q: the engine-live run's claim was reaped — RecoverExpired ran before the renewal set was rebuilt", stdout.String())
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	entry, held := reopened.Lookup("issue-13")
	if !held || entry.RunID != "engine-live-run" {
		t.Fatalf("issue-13 = %+v held=%v, want still held by the engine-live run", entry, held)
	}
	if !entry.ExpiresAt.After(time.Now()) {
		t.Fatalf("issue-13 lease expires %s, want renewed into the future", entry.ExpiresAt)
	}
	if ok, holder, err := reopened.Claim("issue-13", "second-run", "implementation", DefaultClaimLease); err != nil || ok || holder != "engine-live-run" {
		t.Fatalf("second claimant: ok=%v holder=%s err=%v, want refused by the live run's renewed lease", ok, holder, err)
	}
}

// configureEngineInstance appends an `engine:` block to the instance config so
// EngineProjectionEnabled reports true; the probe seam below keeps any test
// from actually dialing the address.
func configureEngineInstance(t *testing.T, root string) {
	t.Helper()
	configFile := instance.NewLayout(root).ConfigFile()
	f, err := os.OpenFile(configFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("\nengine:\n  hostPort: 127.0.0.1:7233\n  namespace: default\n  taskQueue: goobers\n"); err != nil {
		t.Fatal(err)
	}
}

// seedExpiredEngineClaim seeds a lease that EXPIRED while no daemon was
// renewing, held by a run only the engine knows is live — the exact state a
// standalone `goobers run`/`goobers signal` meets on an engine-configured
// instance, since those paths only acquire the instance lock when the daemon
// (and with it every renewal) is stopped.
func seedExpiredEngineClaim(t *testing.T, root, itemID, runID string) {
	t.Helper()
	past := time.Now().Add(-2 * time.Hour)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return past }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim(itemID, runID, "implementation", time.Minute); err != nil || !ok {
		t.Fatalf("seed expired claim: ok=%v err=%v", ok, err)
	}
}

// assertEngineLiveClaimRenewed asserts the one-shot invocation renewed — not
// reaped — the engine-live holder's claim, and that its item cannot be
// claimed over by a new run.
func assertEngineLiveClaimRenewed(t *testing.T, root, itemID, runID string) {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	entry, held := ledger.Lookup(itemID)
	if !held || entry.RunID != runID {
		t.Fatalf("%s = %+v held=%v, want still held by the engine-live run (not reaped)", itemID, entry, held)
	}
	if !entry.ExpiresAt.After(time.Now()) {
		t.Fatalf("%s lease expires %s, want renewed into the future", itemID, entry.ExpiresAt)
	}
	if ok, holder, err := ledger.Claim(itemID, "second-run", "implementation", DefaultClaimLease); err != nil || ok || holder != runID {
		t.Fatalf("second claimant: ok=%v holder=%s err=%v, want refused by the renewed lease", ok, holder, err)
	}
}

// TestOneShotSignalRenewsEngineLiveClaimInsteadOfReaping (#3512 review,
// finding 2): standalone `goobers signal` runs exactly when the daemon is
// down — when renewals have stopped — so on an engine-configured instance its
// setup used to reap every expired-but-engine-live lease (and, gate or not,
// leave it claimable, since Claim treats an expired lease as free). The
// one-shot path must renew the live holder before any scheduling/claiming
// proceeds: claim renewed, not reaped, not claimable by a new run.
func TestOneShotSignalRenewsEngineLiveClaimInsteadOfReaping(t *testing.T) {
	root := initDeterministicDemo(t)
	configureEngineInstance(t, root)

	prevProbe := buildClaimLivenessProbe
	buildClaimLivenessProbe = func(cfg *instance.Config, tracked func() []string) (localscheduler.RunLivenessProbe, func(), error) {
		return localscheduler.CompositeRunLiveness(
			localscheduler.TrackedRunLiveness(tracked),
			stubProbe{"engine-live-run": true},
		), func() {}, nil
	}
	t.Cleanup(func() { buildClaimLivenessProbe = prevProbe })

	seedExpiredEngineClaim(t, root, "issue-14", "engine-live-run")
	code, stdout, stderr := runArgs(t, "signal", "no-subscribed-signal", root)
	if code != 0 {
		t.Fatalf("signal: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	assertEngineLiveClaimRenewed(t, root, "issue-14", "engine-live-run")
}

// TestOneShotRunRenewsEngineLiveClaimInsteadOfReaping is finding 2's
// `goobers run` half: the renewal pass runs before target resolution, so
// even an invocation that goes on to fail (unknown workflow) has already
// renewed — not reaped — the engine-live holder's lease.
func TestOneShotRunRenewsEngineLiveClaimInsteadOfReaping(t *testing.T) {
	root := initDeterministicDemo(t)
	configureEngineInstance(t, root)

	prevProbe := buildClaimLivenessProbe
	buildClaimLivenessProbe = func(cfg *instance.Config, tracked func() []string) (localscheduler.RunLivenessProbe, func(), error) {
		return localscheduler.CompositeRunLiveness(
			localscheduler.TrackedRunLiveness(tracked),
			stubProbe{"engine-live-run": true},
		), func() {}, nil
	}
	t.Cleanup(func() { buildClaimLivenessProbe = prevProbe })

	seedExpiredEngineClaim(t, root, "issue-15", "engine-live-run")
	code, _, stderr := runArgs(t, "run", "no-such-workflow", root)
	if code != 1 || !strings.Contains(stderr, "no workflow named") {
		t.Fatalf("run: code = %d, stderr = %q, want the unknown-workflow failure AFTER claim recovery", code, stderr)
	}
	assertEngineLiveClaimRenewed(t, root, "issue-15", "engine-live-run")
}

// TestOneShotSignalMode1KeepsRecoverAtSetup freezes the pure mode-1 half of
// finding 2's contract: with no `engine:` config the one-shot path builds no
// probe, passes no gate, and the setup-time reap releases the expired lease
// exactly as before.
func TestOneShotSignalMode1KeepsRecoverAtSetup(t *testing.T) {
	root := initDeterministicDemo(t)

	prevProbe := buildClaimLivenessProbe
	buildClaimLivenessProbe = func(*instance.Config, func() []string) (localscheduler.RunLivenessProbe, func(), error) {
		t.Error("a mode-1 one-shot invocation must not build a claim liveness probe")
		return localscheduler.CompositeRunLiveness(), func() {}, nil
	}
	t.Cleanup(func() { buildClaimLivenessProbe = prevProbe })

	seedExpiredEngineClaim(t, root, "issue-16", "long-gone-run")
	code, stdout, stderr := runArgs(t, "signal", "no-subscribed-signal", root)
	if code != 0 {
		t.Fatalf("signal: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := ledger.Lookup("issue-16"); held {
		t.Fatalf("issue-16 = %+v still held, want the expired lease reaped at setup (mode-1 behavior unchanged)", entry)
	}
}
