package engine

import (
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
)

// The two gate/failure routing behaviours plan item E2 ports from the local
// runner: the #415 non-retryable escalation bypass, and the retry-decision
// annotation (plus the knownOutcome shortcut that decides its class).
//
// The POLICY halves — which error codes escalate, and which failures have a
// status-equals outcome knowable without dispatching the checker — are NOT
// re-declared here. They are runner-owned sets (runner.IsNonRetryableEscalation,
// runner.RetryFailureClass) shared through the #624 shared-constant pattern, so
// recognizing a new code on one runner cannot leave the other routing an item
// into a loop it was supposed to bypass.

// taskEscalationTarget lets a workflow intercept a non-retryable task
// disposition through the escalation control branch on its Next gate, mirroring
// internal/runner.taskEscalationTarget. The gate evaluator is deliberately
// bypassed; absent that control branch, the run ends at @escalate.
func taskEscalationTarget(m *wf.Machine, t apiv1.Task) string {
	if nextGate, ok := m.Gate(t.Next); ok {
		if target, ok := wf.BranchTarget(nextGate, wf.BranchEscalate); ok {
			return target
		}
	}
	return wf.TargetEscalate
}

// escalationOutcome routes a non-retryable business disposition (#415) the way
// the local runner's stepTask does: through the Next gate's escalate control
// branch when it declares one, else straight to the escalated terminal.
//
// The terminal STATUS is chosen so the journal PHASE matches the runner's,
// which is what the critic's correction to finding 002 insists any downstream
// hook key on: @abort ends journal.PhaseAborted (StatusBlocked), and BOTH
// @escalate and a terminal-complete escalate branch end journal.PhaseEscalated
// — the runner finishes a complete-targeted disposition as PhaseEscalated, not
// PhaseCompleted, because the item was never done (run.go's `case
// workflow.TerminalComplete: r.finish(..., journal.PhaseEscalated, ...)`).
func escalationOutcome(m *wf.Machine, t apiv1.Task, upstream map[string]apiv1.ResultEnvelope, steps int) (next string, out RunResult, terminal bool) {
	switch target := taskEscalationTarget(m, t); target {
	case wf.TargetAbort:
		return "", RunResult{Status: StatusBlocked, FinalState: t.Name, Outputs: upstream, Steps: steps}, true
	case wf.TargetEscalate, wf.TerminalComplete:
		return "", RunResult{Status: StatusEscalated, FinalState: t.Name, Outputs: upstream, Steps: steps}, true
	default:
		return target, RunResult{}, false
	}
}

// retryDecisionApplies reports whether a resolved gate branch is a repass
// re-entry that owes a retry-decision annotation, mirroring the guard at the
// head of internal/runner.routeRetryDecision: only a non-pass, non-escalated
// branch whose target is a real stage (never a reserved terminal) is a retry
// decision at all.
func retryDecisionApplies(gr gateResult, retryable bool) bool {
	if !retryable || gr.Outcome == gate.OutcomePass || gr.Escalated {
		return false
	}
	switch gr.Target {
	case wf.TargetAbort, wf.TargetEscalate, wf.TerminalComplete:
		return false
	}
	return true
}

// learningEpisodeBranchFor adapts the engine's gate verdict to the canonical
// learning-injection predicate's input (runner.LearningEpisodeAppliesToBranch).
//
// Like retryDecisionApplies above, the SHAPE of the question is ported here
// while the POLICY stays runner-owned: which branches are correctable
// re-entries is one ruling (#3929, extended for agentic outcomes), and a
// second copy of it on this side is a second thing to drift.
func learningEpisodeBranchFor(gr gateResult) runner.LearningEpisodeBranch {
	return runner.LearningEpisodeBranch{
		Outcome: gr.Outcome, Escalated: gr.Escalated, Target: gr.Target, Attempt: gr.Attempt,
	}
}

// retryDecision appends the runner.annotation the local runner writes when a
// fail branch re-enters a completed stage (routeRetryDecision). It carries the
// failure CLASS (policy vs infrastructure), the subject stage's own failure
// code, the cumulative repass attempt, and the target — the shape
// priorRepassCause reads back to tell an infrastructure repass from a content
// one, which is what E6's remediation-evidence routing depends on.
//
// It is a runner.* event, so it is authoritative but never conformance surface
// (journal.IsConformanceNormative): the annotation is deliberately invisible to
// the cross-runner journal diff and is asserted on the raw event log instead.
//
// The runner also checkpoints its machine state here (jr.SetMachineState) so a
// crash between the verdict and the append is recoverable. The engine needs no
// counterpart: Temporal's own history IS that checkpoint, and the walk resumes
// from it by replay rather than by re-reading the journal.
func (r *runJournal) retryDecision(ctx workflow.Context, gr gateResult, stage string, subject apiv1.ResultEnvelope, class journal.AttemptClass) {
	failureCode := ""
	if subject.Error != nil {
		failureCode = subject.Error.Code
	}
	r.append(ctx, journal.Event{
		Type:  journal.EventRunnerAnnotation,
		Stage: stage,
		Gate:  gr.Gate,
		Runner: map[string]interface{}{
			"kind":                      runner.RetryDecisionKind,
			runner.RetryFailureClassKey: string(class),
			"failureCode":               failureCode,
			"repassAttempt":             gr.Attempt,
			"target":                    gr.Target,
		},
	})
}
