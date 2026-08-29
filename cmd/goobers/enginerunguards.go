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

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
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
	if g == nil || g.client == nil {
		return engineRunAttachment{Err: fmt.Errorf("re-attach to engine run %s: %w", runID, errNoEngineClient)}
	}
	describeCtx, cancel := context.WithTimeout(ctx, engineDescribeTimeout)
	desc, err := g.client.DescribeWorkflowExecution(describeCtx, runID, "")
	cancel()
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return engineRunAttachment{}
		}
		return engineRunAttachment{Err: fmt.Errorf("describe engine run %s: %w", runID, err)}
	}
	attachment := engineRunAttachment{Found: true, Status: desc.GetWorkflowExecutionInfo().GetStatus()}
	// The wait deliberately runs on the caller's context, NOT a bounded one:
	// an engine run legitimately outlives the daemon that started it, and a
	// premature timeout here would put the daemon right back into "I do not
	// know how this run ended".
	if err := g.client.GetWorkflow(ctx, runID, "").Get(ctx, nil); err != nil {
		attachment.Err = err
	}
	return attachment
}

// cancel asks the engine to cancel an engine-driven run's workflow. It is the
// stall sweep's replacement for terminalizing the journal file: writing a
// terminal event into a journal whose workflow is still executing leaves the
// engine driving a run the daemon believes is over, which is the same
// two-drivers hazard from the other end.
func (g *engineRunGuards) cancel(ctx context.Context, runID string) error {
	if g == nil || g.client == nil {
		return fmt.Errorf("cancel engine run %s: %w", runID, errNoEngineClient)
	}
	cancelCtx, cancel := context.WithTimeout(ctx, engineCancelTimeout)
	defer cancel()
	if err := g.client.CancelWorkflow(cancelCtx, runID, ""); err != nil {
		return fmt.Errorf("cancel engine run %s: %w", runID, err)
	}
	return nil
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
	if deps.release != nil {
		deps.release(id.RunID, id.Workflow)
	}
	ingestRunTelemetry(deps.telemetry, deps.rollupDB, deps.watermarks, deps.layout, id.RunID, deps.log)
	if deps.log == nil {
		return
	}
	// The status observed at describe time distinguishes "the workflow was
	// still running and this daemon waited it out" from "it had already closed
	// while we were down" — the two shapes an operator reading the startup log
	// wants to tell apart when a restart is followed by a terminal echo.
	ev := journal.Event{
		Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: id.RunID,
		Reason: "engine workflow " + strings.ToLower(strings.TrimPrefix(attachment.Status.String(), "WORKFLOW_EXECUTION_STATUS_")),
	}
	switch {
	case attachment.Err != nil && !attachment.Found:
		ev.Type = journal.EventError
		ev.Error = &journal.ErrorDetail{
			Code:    "engine_run_reattach_failed",
			Message: fmt.Sprintf("engine-driven run %s could not be re-attached: %v", id.RunID, attachment.Err),
		}
	case !attachment.Found:
		ev.Type = journal.EventError
		ev.Error = &journal.ErrorDetail{
			Code: "engine_run_unresolvable",
			Message: fmt.Sprintf(
				"engine-driven run %s has no workflow on the engine; neither resumed nor terminalized", id.RunID),
		}
	case attachment.Err != nil:
		ev.Type = journal.EventRunFinished
		ev.Status = "error: " + attachment.Err.Error()
	default:
		ev.Type = journal.EventRunFinished
		ev.Status = string(engineRunTerminalPhase(deps.layout, id.RunID))
	}
	_ = deps.log.Append(ev)
}

// engineRunTerminalPhase reads back the phase the engine's own journal writer
// recorded once its workflow closed. A run whose journal has not been
// projected yet (the reconciler runs on its own interval) still reads
// PhaseRunning, and the echo reports completed — the workflow returned
// without error, which is the only thing this process actually observed.
func engineRunTerminalPhase(l instance.Layout, runID string) journal.RunPhase {
	rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return journal.PhaseCompleted
	}
	phase, err := rd.Phase()
	if err != nil || phase == journal.PhaseRunning {
		return journal.PhaseCompleted
	}
	return phase
}

// engineDrivenRefusal is the named error every operator path returns for a
// run the engine drives. The message names the driver and the reason rather
// than the mechanism that failed, because the operator's next question is
// always "then how do I stop it".
func engineDrivenRefusal(runID, action string) error {
	return fmt.Errorf(
		"run %s is engine-driven (run.yaml driver: %s): %s would edit a journal the engine still owns "+
			"while its workflow keeps executing; act on the engine's workflow instead",
		runID, journal.DriverEngine, action)
}
