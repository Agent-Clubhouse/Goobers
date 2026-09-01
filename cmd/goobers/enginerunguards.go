package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
	telemetryingest "github.com/goobers/goobers/internal/telemetry/ingest"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// enginerunguards.go holds the daemon's guards for runs it does not drive.
//
// A run whose run.yaml carries `driver: engine` (journal.DriverEngine) is
// walked by the tier-3 engine on Temporal, not by this process's
// internal/runner. Every daemon path that acts on a non-terminal run —
// the startup resume scan, the stall sweep, `run cancel`/`run abort`, and the
// HITL intervention service — was written when the only driver was the local
// runner, so each of them will happily take over a run the engine is still
// executing. That is a duplicate-driver hazard, not a cosmetic one: two
// drivers on one run means two attempts at open-pr, push-branch and merge-pr.
//
// The guards here are the daemon's half of decision 003's "Phase-0
// engine-start hygiene": read the driver, and for an engine-driven run reach
// the engine over Temporal (re-attach, or cancel) instead of touching the
// journal in-process.

const (
	// engineDescribeTimeout bounds one describe so a wedged Temporal frontend
	// degrades a single re-attachment rather than the whole resume scan.
	engineDescribeTimeout = 15 * time.Second
	// engineCancelTimeout bounds one CancelWorkflow for the same reason: the
	// stall sweep runs on the daemon's periodic ticker and must return.
	engineCancelTimeout = 15 * time.Second
)

// engineDescribeRetryBudget bounds how long ONE re-attachment keeps retrying a
// describe that failed for a reason other than NotFound, and
// engineDescribeRetryInitial/Max are the backoff within that budget.
//
// The retry exists because the two events most likely to interrupt an engine
// run — a goobers-api rollout and a Temporal frontend rollout — are the ones
// an operator performs together, and a single Unavailable during that window
// used to end the attachment permanently. The re-attachment goroutine already
// outlives the whole run by design, so minutes of patience here cost nothing;
// what it buys is that the daemon does not conclude "outcome unknown" from one
// bad RPC and free the run's concurrency slot underneath a live workflow.
//
// Vars rather than consts so tests can drive the give-up path in milliseconds.
var (
	engineDescribeRetryBudget  = 5 * time.Minute
	engineDescribeRetryInitial = 2 * time.Second
	engineDescribeRetryMax     = 30 * time.Second
)

// engineWorkflowClient is the slice of the Temporal client these guards need,
// mirroring internal/engine's own workflowLivenessClient: client.Client
// satisfies it, and tests substitute a fake so no Temporal server is needed
// to prove the daemon-side decisions.
type engineWorkflowClient interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	GetWorkflow(ctx context.Context, workflowID, runID string) client.WorkflowRun
	CancelWorkflow(ctx context.Context, workflowID, runID string) error
}

// dialDaemonEngine is the daemon's single Temporal dial, a var so tests can
// substitute a fake client without a server.
var dialDaemonEngine = bootstrap.DialTemporal

// daemonEngineClient is the ONE Temporal client a `goobers up` process owns.
// The projection reconciler, the DS6 claim-liveness probe and the run guards
// below all speak to the same frontend for the same instance, and each used
// to dial its own connection — three TCP connections, three independent
// failure modes, and three places an operator has to look when the daemon
// cannot reach Temporal. up.go now dials once, hands the client to each
// consumer, and closes it on shutdown; a consumer never closes it.
//
// nil means this instance has no `engine:` configuration, which is the
// type-1/type-2 topology: no client is dialed, and every guard degrades to
// its pre-existing behaviour because no run can be engine-driven.
type daemonEngineClient struct {
	client    client.Client
	namespace string
}

// newDaemonEngineClient dials the daemon's shared Temporal client, or returns
// nil when this instance runs no engine.
func newDaemonEngineClient(cfg *instance.Config) (*daemonEngineClient, error) {
	if cfg == nil || !cfg.EngineProjectionEnabled() {
		return nil, nil
	}
	engineConfig := cfg.EffectiveEngineConfig()
	c, err := dialDaemonEngine(engineConfig.HostPort, engineConfig.Namespace)
	if err != nil {
		return nil, fmt.Errorf("dial temporal at %s: %w", engineConfig.HostPort, err)
	}
	return &daemonEngineClient{client: c, namespace: engineConfig.Namespace}, nil
}

// Temporal returns the shared client, or nil when no engine is configured.
func (e *daemonEngineClient) Temporal() client.Client {
	if e == nil {
		return nil
	}
	return e.client
}

// Namespace is the Temporal namespace the shared client is bound to.
func (e *daemonEngineClient) Namespace() string {
	if e == nil {
		return ""
	}
	return e.namespace
}

// Guards returns the engine-driven run guards over the shared client. A nil
// *engineRunGuards is a valid value with defined behaviour (see the methods
// below), so a type-1 daemon threads nil everywhere instead of branching.
func (e *daemonEngineClient) Guards() *engineRunGuards {
	if e == nil || e.client == nil {
		return nil
	}
	return &engineRunGuards{client: e.client}
}

// HITLDeliverer returns the #3883 operator-intent deliverer over the shared
// client, or nil when no engine is configured — in which case the daemon's
// intervention surface keeps the #3847 engine-driven refusal.
//
// It takes the guards' workflow-id resolver rather than owning one: a
// scheduled engine run's workflow id is not its run id, and an operator intent
// addressed to the run id would be delivered to a workflow that does not
// exist. Passing the guards in makes the two surfaces share one boot-time scan
// and one answer to "which workflow is this run?".
func (e *daemonEngineClient) HITLDeliverer(guards *engineRunGuards) *engine.HITLDeliverer {
	if e == nil || e.client == nil {
		return nil
	}
	deliverer, err := engine.NewHITLDeliverer(e.client)
	if err != nil {
		return nil
	}
	if guards == nil || guards.resolveWorkflowID == nil {
		return deliverer
	}
	return deliverer.WithWorkflowIDResolver(guards.resolveWorkflowID)
}

// Close releases the shared client.
func (e *daemonEngineClient) Close() {
	if e == nil || e.client == nil {
		return
	}
	e.client.Close()
}

// engineRunGuards is the daemon-side control surface over engine-driven runs.
type engineRunGuards struct {
	client engineWorkflowClient
	// resolveWorkflowID maps a Goobers run id to the Temporal workflow id it
	// actually executes under, for the case where the two differ.
	//
	// A DIRECT engine run's workflow id IS its run id, so every guard here
	// addresses it by run id alone and never calls this at all — that fast
	// path is the common case after #3876 and stays free. A SCHEDULED engine
	// run's is not: internal/engine's RunScheduled hashes the claim workflow's
	// id into the run id, so describing the run id yields NotFound and the
	// daemon concludes "no workflow is addressable as this run" for a run that
	// is very much executing. Before #3876 that was a named, accepted gap
	// (decision 003's engine_run_unresolvable); it is not survivable once real
	// triggers put scheduled runs on the engine, because NotFound is treated
	// as SETTLED and settlement releases the scheduler's concurrency slot
	// underneath a live workflow.
	//
	// It is a func, populated at boot from engine.WorkflowLiveness's cached
	// open-workflow scan, because the mapping only exists on the far side.
	// nil (no engine, or a scan that could not be started) leaves the
	// pre-#3876 behaviour exactly in place. Its errors are load-bearing:
	// engine.ErrRunNotOpen is DEFINITE (nothing is driving the run), anything
	// else — an enumeration that failed, engine.ErrAmbiguousRunID — is
	// UNKNOWN and must not be read as settlement.
	resolveWorkflowID func(ctx context.Context, runID string) (string, error)
}

// withWorkflowIDResolver returns guards that consult resolve when a describe
// (or a cancel) comes back NotFound. Returns g unchanged when either is nil.
func (g *engineRunGuards) withWorkflowIDResolver(resolve func(ctx context.Context, runID string) (string, error)) *engineRunGuards {
	if g == nil || resolve == nil {
		return g
	}
	next := *g
	next.resolveWorkflowID = resolve
	return &next
}

// errEngineRunUnresolvable is what a guard reports when NOTHING open on the
// engine is addressable as a run id — the definite half of a NotFound. It is
// distinct from a resolution that merely failed, because only this one means
// the run is over.
var errEngineRunUnresolvable = errors.New("no open engine workflow is addressable as this run")

// resolveEngineWorkflowID answers which workflow id to retry a NotFound
// against, or why the daemon still cannot say.
//
// Three outcomes, and every caller must keep them apart:
//
//   - (id, nil): the run executes as id. Retry against it.
//   - ("", errEngineRunUnresolvable): DEFINITE — nothing on the engine is
//     driving this run. Settling it is correct.
//   - ("", anything else): UNKNOWN. Hold the slot, report, retry next tick.
//
// With no resolver the answer is the pre-#3876 one: unresolvable. That is
// what a type-1 daemon, or a daemon whose boot scan never ran, has always
// concluded from a NotFound, and it stays correct for every direct run.
func (g *engineRunGuards) resolveEngineWorkflowID(ctx context.Context, runID string) (string, error) {
	if g == nil || g.resolveWorkflowID == nil {
		return "", errEngineRunUnresolvable
	}
	workflowID, err := g.resolveWorkflowID(ctx, runID)
	switch {
	case errors.Is(err, engine.ErrRunNotOpen):
		return "", errEngineRunUnresolvable
	case err != nil:
		return "", fmt.Errorf("resolve engine run %s to a workflow id: %w", runID, err)
	case workflowID == "" || workflowID == runID:
		// The inverse answered with the id we already described. Nothing new
		// to try, so the NotFound stands.
		return "", errEngineRunUnresolvable
	default:
		return workflowID, nil
	}
}

// errNoEngineClient is what every guard reports when it is asked to act on an
// engine-driven run from a process with no Temporal client. It is deliberately
// an error rather than a silent fallback to the local-runner path: an
// engine-driven journal on an instance that cannot reach the engine is a
// misconfiguration, and the pre-guard behaviour (drive it, or terminalize it)
// is exactly the corruption these guards exist to prevent.
var errNoEngineClient = errors.New("this daemon has no engine client configured")

// engineRunAttachment is what one re-attachment learned.
type engineRunAttachment struct {
	// Found is true when a workflow exists under the run id. False means the
	// engine has no record of it — history retention expired, or (for a
	// scheduled engine run) the workflow id is not the run id at all. Either
	// way the daemon must not drive the run itself.
	Found bool
	// Settled is the load-bearing one: the daemon POSITIVELY established that
	// the run is no longer executing on the engine. Only a settled attachment
	// may do the bookkeeping that assumes the run is over — releasing the
	// scheduler's reconciled concurrency slot above all, because
	// ReleaseReconciled is one-way (localscheduler.Scheduler.Reconcile's
	// contract: "MUST call ReleaseReconciled once each one's outcome is
	// known") and its wakeForDemand can admit a second run of the same
	// workflow the instant it lands.
	//
	// An unsettled attachment is "I could not find out": a describe that kept
	// failing for a reason other than NotFound, or a Get that failed for a
	// transport reason rather than reporting the workflow's own outcome. The
	// run is presumed still executing, and this daemon holds its slot.
	Settled bool
	// Status is Temporal's execution status at describe time.
	Status enumspb.WorkflowExecutionStatus
	// Err is a describe or await failure. A workflow that itself failed
	// reports its failure here too: the daemon only echoes the outcome, the
	// engine owns the run's journal either way.
	Err error
}

// await describes the run's workflow and, when it exists, blocks until it
// closes. Get on an already-closed workflow returns immediately, so one call
// covers both "still running" and "finished while we were down".
func (g *engineRunGuards) await(ctx context.Context, runID string) engineRunAttachment {
	return g.awaitInto(ctx, runID, nil)
}

// awaitInto is await with the workflow's return value decoded into out.
//
// The resume scan does not care what a run RETURNED — it only needs to know
// the run is over so it can release the slot and echo a terminal. The engine
// starter (decision 005 D1) needs the engine.RunResult itself: the phase it
// reports to the scheduler, the NoWork flag that drives idle backoff, and the
// per-stage outcome envelopes the terminal-hook frame keys on all come from
// there. Same wait, same settlement rules, one optional out parameter, so the
// two callers cannot end up with two different notions of "settled".
func (g *engineRunGuards) awaitInto(ctx context.Context, runID string, out any) engineRunAttachment {
	if g == nil || g.client == nil {
		return engineRunAttachment{Err: fmt.Errorf("re-attach to engine run %s: %w", runID, errNoEngineClient)}
	}
	// The run's OWN id first: TemporalStarter.Start uses WorkflowID == RunID,
	// so for every direct run this describe answers and the open-workflow
	// inverse is never consulted, never paged, never billed.
	workflowID := runID
	desc, err := g.describe(ctx, workflowID)
	if err != nil {
		var notFound *serviceerror.NotFound
		if !errors.As(err, &notFound) {
			return engineRunAttachment{Err: fmt.Errorf("describe engine run %s: %w", runID, err)}
		}
		// No workflow under this run id. Before concluding anything, ask the
		// open-workflow inverse: a SCHEDULED run's workflow id is not its run
		// id (#3877), and NotFound is the normal answer for one that is
		// executing perfectly well.
		resolved, resolveErr := g.resolveEngineWorkflowID(ctx, runID)
		switch {
		case errors.Is(resolveErr, errEngineRunUnresolvable):
			// Settled: nothing on the engine is addressable as this run, so
			// no describe will ever answer differently and holding the slot
			// forever would turn one unresolvable run into a permanent
			// concurrency outage for its workflow.
			return engineRunAttachment{Settled: true}
		case resolveErr != nil:
			// UNKNOWN, not settled: an enumeration that failed, or a run id
			// two workflows claim. Either way the daemon has not established
			// that this run is over, and releasing its slot on a maybe is the
			// duplicate-driver hazard reached from the recovery path.
			return engineRunAttachment{Err: resolveErr}
		}
		workflowID = resolved
		if desc, err = g.describe(ctx, workflowID); err != nil {
			if errors.As(err, &notFound) {
				// The scan named a workflow that has since closed and been
				// swept from visibility. That IS settlement: it ran, and it
				// is no longer running.
				return engineRunAttachment{Settled: true}
			}
			return engineRunAttachment{Err: fmt.Errorf("describe engine run %s as workflow %s: %w", runID, workflowID, err)}
		}
	}
	attachment := engineRunAttachment{Found: true, Status: desc.GetWorkflowExecutionInfo().GetStatus()}
	// A workflow already closed at describe time is settled by the describe
	// alone, whatever the wait below reports.
	attachment.Settled = attachment.Status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	// The wait deliberately runs on the caller's context, NOT a bounded one:
	// an engine run legitimately outlives the daemon that started it, and a
	// premature timeout here would put the daemon right back into "I do not
	// know how this run ended".
	if err := g.client.GetWorkflow(ctx, workflowID, "").Get(ctx, out); err != nil {
		attachment.Err = err
		// A failed, cancelled, terminated or timed-out workflow reports its
		// own outcome through this error — that IS the answer, and the run is
		// over. Anything else (an RPC the poll could not complete) is not.
		attachment.Settled = attachment.Settled || engineOutcomeError(err)
		return attachment
	}
	attachment.Settled = true
	return attachment
}

// describe runs one bounded DescribeWorkflowExecution, retrying anything that
// is not NotFound within engineDescribeRetryBudget. NotFound and context
// cancellation return immediately: neither gets better by waiting.
func (g *engineRunGuards) describe(ctx context.Context, workflowID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	deadline := time.Now().Add(engineDescribeRetryBudget)
	backoff := engineDescribeRetryInitial
	for {
		describeCtx, cancel := context.WithTimeout(ctx, engineDescribeTimeout)
		desc, err := g.client.DescribeWorkflowExecution(describeCtx, workflowID, "")
		cancel()
		if err == nil {
			return desc, nil
		}
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) || ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > engineDescribeRetryMax {
			backoff = engineDescribeRetryMax
		}
	}
}

// engineOutcomeError reports whether err is the workflow telling us how it
// ended, rather than the client failing to find out.
func engineOutcomeError(err error) bool {
	var executionErr *temporal.WorkflowExecutionError
	if errors.As(err, &executionErr) {
		return true
	}
	return temporal.IsCanceledError(err) || temporal.IsTerminatedError(err) ||
		temporal.IsTimeoutError(err) || temporal.IsApplicationError(err)
}

// cancel asks the engine to cancel an engine-driven run's workflow. It is the
// stall sweep's replacement for terminalizing the journal file — and, since
// #3877, what `goobers run cancel`/`run abort` route to for an engine-driven
// run instead of refusing. Writing a terminal event into a journal whose
// workflow is still executing leaves the engine driving a run the daemon
// believes is over, which is the same two-drivers hazard from the other end.
func (g *engineRunGuards) cancel(ctx context.Context, runID string) error {
	_, err := g.cancelResolved(ctx, runID)
	return err
}

// cancelResolved is cancel, reporting WHICH Temporal workflow it cancelled.
// The operator paths print it: for a scheduled run the workflow id is the
// only handle an operator has to go confirm the cancellation in Temporal, and
// it is precisely the id they could not have derived themselves.
//
// A NotFound cancel is resolved through the open-workflow inverse exactly as
// a NotFound describe is, and for the same reason: a scheduled run's RunID is
// a hash of its claim workflow's id, so cancelling by run id addresses
// nothing. An unresolvable run is reported (wrapping
// errEngineRunUnresolvable) rather than being downgraded to success — a
// cancel that cancelled nothing must never read as one that landed.
func (g *engineRunGuards) cancelResolved(ctx context.Context, runID string) (string, error) {
	if g == nil || g.client == nil {
		return "", fmt.Errorf("cancel engine run %s: %w", runID, errNoEngineClient)
	}
	err := g.cancelWorkflow(ctx, runID)
	if err == nil {
		return runID, nil
	}
	var notFound *serviceerror.NotFound
	if !errors.As(err, &notFound) {
		return "", fmt.Errorf("cancel engine run %s: %w", runID, err)
	}
	workflowID, resolveErr := g.resolveEngineWorkflowID(ctx, runID)
	if resolveErr != nil {
		return "", fmt.Errorf("cancel engine run %s: %w", runID, resolveErr)
	}
	if err := g.cancelWorkflow(ctx, workflowID); err != nil {
		return "", fmt.Errorf("cancel engine run %s as workflow %s: %w", runID, workflowID, err)
	}
	return workflowID, nil
}

// cancelWorkflow runs one bounded CancelWorkflow. The stall sweep runs on the
// daemon's periodic ticker and must return.
func (g *engineRunGuards) cancelWorkflow(ctx context.Context, workflowID string) error {
	cancelCtx, cancel := context.WithTimeout(ctx, engineCancelTimeout)
	defer cancel()
	return g.client.CancelWorkflow(cancelCtx, workflowID, "")
}

// engineReattachDeps is the daemon state one re-attachment needs after the
// wait returns. It mirrors, field for field, what the resume scan's own
// goroutine closes over for a runner-driven run — the two paths differ only
// in who walks the stages.
type engineReattachDeps struct {
	layout     instance.Layout
	log        *journal.InstanceLog
	telemetry  *telemetry.Client
	rollupDB   *rollup.DB
	watermarks *intake.Store
	release    func(runID, workflow string)
}

// reattachEngineRun waits for an interrupted engine-driven run's workflow and
// then performs exactly the bookkeeping the daemon owes: release the
// scheduler's reconciled concurrency slot, ingest the run's telemetry, and
// echo a terminal run.finished into the instance log. It never appends to the
// run's own journal — the engine (through the live journal plane, or the
// projection reconciler) is that journal's only writer.
//
// It is called from a goroutine that is deliberately NOT tracked by the
// daemon's drain WaitGroup and NOT registered with the runner registry:
// the daemon's shutdown drain waits for runs THIS process is executing, and
// HardStopAll stops them. An engine run is neither — waiting for it would
// hold SIGTERM open for the run's whole duration, and hard-stopping it is not
// even meaningful, since nothing in this process is driving it.
func reattachEngineRun(ctx context.Context, guards *engineRunGuards, id journal.RunIdentity, deps engineReattachDeps) {
	attachment := guards.await(ctx, id.RunID)
	// The daemon is shutting down: the run is still the engine's, and the
	// next daemon's resume scan re-attaches to it. Doing bookkeeping now
	// would race the shutdown that is already closing the instance log, the
	// rollup DB and the scheduler this function is about to write to.
	if ctx.Err() != nil {
		return
	}
	// Both of these assume the run is OVER. Releasing the reconciled slot is
	// irreversible for the life of the daemon and immediately invites the
	// scheduler to admit another run of the same workflow; ingesting
	// telemetry publishes a terminal outcome to the rollup. Doing either for
	// a run whose status this daemon never established is the duplicate-work
	// hazard the guards exist to close, reached from the failure path.
	//
	// The unresolvable case (`!Found`) IS settled and does release: no
	// workflow is addressable under this run id, so nothing on the engine can
	// be driving it. The scheduled-run exception this comment used to name —
	// a run whose RunID is a hash of its claim workflow's id and therefore
	// never describes — is closed as of #3876/#3877: a NotFound describe is
	// resolved through engine.WorkflowLiveness's open-workflow inverse before
	// it may settle anything, so a scheduled engine run that is still open is
	// found and waited on rather than released. A resolution that could not
	// complete (visibility down, or an ambiguous run id) is UNKNOWN and lands
	// in the `!Settled` arm below, holding the slot.
	if attachment.Settled {
		if deps.release != nil {
			deps.release(id.RunID, id.Workflow)
		}
		telemetryingest.RunTelemetry(deps.telemetry, deps.rollupDB, deps.watermarks, deps.layout, id.RunID, deps.log)
	}
	if deps.log == nil {
		return
	}
	// The status observed at describe time distinguishes "the workflow was
	// still running and this daemon waited it out" from "it had already closed
	// while we were down" — the two shapes an operator reading the startup log
	// wants to tell apart when a restart is followed by a terminal echo.
	ev := journal.Event{Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: id.RunID}
	switch {
	case !attachment.Settled:
		ev.Type = journal.EventError
		ev.Reason = "engine workflow status unknown"
		ev.Error = &journal.ErrorDetail{
			Code:    "engine_run_reattach_failed",
			Message: fmt.Sprintf("engine-driven run %s could not be re-attached: %v", id.RunID, attachment.Err),
		}
	case !attachment.Found:
		ev.Type = journal.EventError
		ev.Reason = "engine has no workflow under this run id"
		// The named recovery action rides the error event rather than a second
		// append: an operator's log scraper greps the code, and the action
		// vocabulary (RecoveryActionReattached on the way in) stays complete
		// on the way out.
		ev.Runner = map[string]any{
			"action": journal.RecoveryActionUnresolved,
			"driver": string(journal.DriverEngine),
		}
		ev.Error = &journal.ErrorDetail{
			Code: "engine_run_unresolvable",
			Message: fmt.Sprintf(
				"engine-driven run %s has no workflow on the engine; neither resumed nor terminalized", id.RunID),
		}
	case attachment.Err != nil:
		// The workflow reported its own failure. Echo it as a REAL terminal
		// phase: journal.RunPhase is what readmodel's projection switches on,
		// and a free-text status (the old "error: <err>") is silently dropped
		// by its terminalPhase guard — losing the echo in exactly the cases an
		// operator most wants it.
		ev.Type = journal.EventRunFinished
		ev.Reason = engineWorkflowReason(attachment.Status) + ": " + attachment.Err.Error()
		ev.Status = string(engineRunTerminalPhase(deps.layout, id.RunID, journal.PhaseFailed))
	default:
		ev.Type = journal.EventRunFinished
		ev.Reason = engineWorkflowReason(attachment.Status)
		ev.Status = string(engineRunTerminalPhase(deps.layout, id.RunID, journal.PhaseCompleted))
	}
	_ = deps.log.Append(ev)
}

// engineWorkflowReason renders Temporal's execution status for the instance
// log. WorkflowExecutionStatus.String() already yields "Running"/"Completed"
// (the enum's Go names, not its WORKFLOW_EXECUTION_STATUS_* wire constants).
func engineWorkflowReason(status enumspb.WorkflowExecutionStatus) string {
	return "engine workflow " + strings.ToLower(status.String())
}

// engineRunTerminalPhase reads back the phase the engine's own journal writer
// recorded once its workflow closed, falling back to fallback when the
// journal has not been projected yet (the reconciler runs on its own
// interval) — the phase this process actually observed the workflow reach.
func engineRunTerminalPhase(l instance.Layout, runID string, fallback journal.RunPhase) journal.RunPhase {
	rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return fallback
	}
	phase, err := rd.Phase()
	if err != nil || phase == journal.PhaseRunning {
		return fallback
	}
	return phase
}

// engineRunSettledOnDisk reports whether a run's own journal already carries
// a terminal event. It is the precondition the operator paths check BEFORE
// refusing an engine-driven run: a closed journal has nothing left for either
// driver to write, so the accurate answer is each command's existing "already
// terminal", not a refusal that sends the operator hunting for a workflow
// that finished (or was never there).
func engineRunSettledOnDisk(reader *journal.Reader) bool {
	if reader == nil {
		return false
	}
	phase, err := reader.Phase()
	if err != nil {
		return false
	}
	return isTerminalPhase(phase)
}

// engineDrivenRefusal is the named error the HITL intervention path returns
// for a run the engine drives. The message names the driver and the reason
// rather than the mechanism that failed, because the operator's next question
// is always "then how do I stop it".
//
// `run cancel`/`run abort` no longer return this: as of #3877 they route to
// CancelWorkflow instead of refusing. An intervention still does, and must —
// approving or overriding a gate is not "stop the run", it is a WRITE into a
// journal whose only writer is the engine's workflow, and there is no
// equivalent engine-side operation to route it to.
//
// It deliberately does NOT assert that the run's workflow is still executing:
// the intervention service has no Temporal client to check with, and for a
// run whose workflow has vanished that claim would be false. What is true in
// every case is that the engine, not this process, is the journal's writer —
// so the remedy is stated as the workflow, plus the commands that reach it.
func engineDrivenRefusal(runID, action string) error {
	return fmt.Errorf(
		"run %s is engine-driven (run.yaml driver: %s): %s would edit a journal whose only writer is the "+
			"engine's workflow; cancel that workflow with `goobers run cancel %s` (or let the daemon's stalled-run "+
			"sweep cancel it) instead",
		runID, journal.DriverEngine, action, runID)
}
