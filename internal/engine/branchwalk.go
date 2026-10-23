package engine

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runsemantics"
	wf "github.com/goobers/goobers/internal/workflow"
)

// branchWalkFrame is an internal continuation of the task/gate walk. It is
// assembled only by tests until the static controller owns branch admission,
// lifetimes and joining. There is no serialized input or production switch.
// The walk copies seed collections before mutation, then stores its own
// continuation back into the frame; siblings never write into the seed.
type branchWalkFrame struct {
	visited         map[string]bool
	results         map[string]apiv1.ResultEnvelope
	bind            func(fanOutBindingContext) (fanOutBindings, error)
	started         livejournal.AppendReceipt
	start           string
	deadline        time.Time
	upstream        map[string]apiv1.ResultEnvelope
	evaluatedGates  map[string]bool
	pointers        []apiv1.ContextPointer
	completed       completedStages
	lastStage       string
	lastResult      apiv1.ResultEnvelope
	workspaceBranch string
	continuity      []continuityEntry

	status         journal.BranchStatus
	terminalTarget string
	produced       bool
	noOutput       bool
}

func (r *runJournal) branchBoundary(ctx workflow.Context, state string, upstream map[string]apiv1.ResultEnvelope, steps int) (RunResult, error, bool) {
	f := r.branchWalk
	if f == nil {
		return RunResult{}, nil, false
	}
	if state == wf.TargetJoin {
		return RunResult{Status: StatusCompleted, Outputs: upstream, Steps: steps}, nil, true
	}
	if state == wf.TerminalComplete {
		return RunResult{}, fmt.Errorf("engine: branch completed the run instead of routing to %q", wf.TargetJoin), true
	}
	err := f.boundary(ctx)
	return RunResult{}, err, err != nil
}

func (f *branchWalkFrame) boundary(ctx workflow.Context) error {
	if err := ctx.Err(); err != nil {
		f.status = journal.BranchCancelled
		return err
	}
	if !f.deadline.IsZero() && !workflow.Now(ctx).Before(f.deadline) {
		f.status = journal.BranchTimedOut
		return fmt.Errorf("engine: branch execution budget expired at stage boundary")
	}
	return nil
}

func (f *branchWalkFrame) finish(ctx workflow.Context, out RunResult, err error, pointers []apiv1.ContextPointer) {
	if ctx.Err() != nil {
		f.status = journal.BranchCancelled
	}
	if f.status != "" {
		return
	}
	switch {
	case err != nil:
		f.status = journal.BranchFailed
	case out.Status == StatusBlocked:
		f.status, f.terminalTarget = journal.BranchFailed, wf.TargetAbort
	case out.Status == StatusEscalated:
		f.status, f.terminalTarget = journal.BranchFailed, wf.TargetEscalate
	case out.Status != StatusCompleted:
		f.status = journal.BranchFailed
	default:
		f.status = runsemantics.SettledBranchStatus(false, false, f.noOutput, f.produced || len(pointers) > len(f.pointers))
	}
}

// walkTaskOutcome preserves the runner's branch rules. A failure without a
// following gate fails this branch; an authored run terminal is retained for
// the future controller to apply after it cancels and joins the other branches.
func walkTaskOutcome(ctx workflow.Context, m *wf.Machine, t apiv1.Task, result apiv1.ResultEnvelope, upstream map[string]apiv1.ResultEnvelope, evaluated map[string]bool, steps int, rec *runJournal) (string, RunResult, bool) {
	f := rec.branchWalk
	if f == nil {
		return taskOutcome(ctx, m, t, result, upstream, evaluated, steps, rec)
	}
	if f.visited == nil {
		f.visited = make(map[string]bool)
	}
	f.visited[t.Name] = true
	if f.results == nil {
		f.results = make(map[string]apiv1.ResultEnvelope)
	}
	if result.Status == apiv1.ResultFailure && t.ContinueOnError {
		delete(f.results, t.Name)
	} else {
		f.results[t.Name] = result
	}
	f.produced = f.produced || len(result.Outputs) > 0 || len(result.Artifacts) > 0
	if result.Status == apiv1.ResultNoWork {
		f.noOutput = true
	}
	if result.Status == apiv1.ResultFailure && !t.ContinueOnError {
		if _, isGate := m.Gate(t.Next); !isGate {
			code, message := failureCause(result.Error)
			return "", RunResult{Status: StatusFailed, FinalState: t.Name, FailureCode: code, FailureMessage: message, Outputs: upstream, Steps: steps}, true
		}
	}
	return taskOutcome(ctx, m, t, result, upstream, evaluated, steps, rec)
}

func (r *runJournal) branchGateJoinFailure(gr gateResult, lastStage string, lastResult apiv1.ResultEnvelope, upstream map[string]apiv1.ResultEnvelope, steps int) (RunResult, bool) {
	if r.branchWalk == nil || gr.Target != wf.TargetJoin || lastResult.Status != apiv1.ResultFailure || gr.Outcome == gate.OutcomePass {
		return RunResult{}, false
	}
	code, message := failureCause(lastResult.Error)
	return RunResult{Status: StatusFailed, FinalState: lastStage, FailureCode: code, FailureMessage: message, Outputs: upstream, Steps: steps}, true
}

// A gate can compile with one @join edge and a different edge that completes
// the run. Check the selected edge before gateTransition consumes it; the
// branch loop never sees a terminal already returned by that function.
func (r *runJournal) refuseBranchGateCompletion(gr gateResult) error {
	if r.branchWalk != nil && gr.Target == wf.TerminalComplete {
		return fmt.Errorf("engine: branch gate %q completed the run instead of routing to %q", gr.Gate, wf.TargetJoin)
	}
	return nil
}
