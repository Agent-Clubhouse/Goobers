package engine

import (
	"fmt"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
)

// settleWalk is the terminal hook for the serial walk. Branch bodies return
// their outcome to the future static controller without opening a HITL hold.
func settleWalk(ctx workflow.Context, rec *runJournal, hitl *hitlSession, out RunResult, addenda map[string]string) (string, bool, error) {
	if rec.branchWalk != nil {
		return "", false, nil
	}
	plan, resumed, _, err := hitl.settle(ctx, out)
	if err != nil || !resumed {
		return "", false, err
	}
	if plan.stage != "" && plan.addendum != "" {
		addenda[plan.stage] = plan.addendum
	}
	return plan.state, true, nil
}

// gateContinuation passes the committed verdict and any learning episode to
// the next stage. This preserves the existing command order on serial walks.
func (r *runJournal) gateContinuation(ctx workflow.Context, in RunInput, gateName, next string, gr gateResult, verdict *apiv1.Verdict, lastStage string, lastResult apiv1.ResultEnvelope, verdictArtifact *apiv1.ArtifactPointer, pointers []apiv1.ContextPointer) ([]apiv1.ContextPointer, error) {
	var injected *apiv1.ContextPointer
	if verdictArtifact != nil {
		// #412: the next dispatch — most commonly a repass back to the
		// stage that produced the subject this gate just evaluated —
		// must actually receive the reviewer's verdict as context, not
		// infer "something needs to change" from git. The local
		// runner's walk appends the same "<gate>.verdict" pointer on
		// both its retry route and its advance path.
		pointer := apiv1.ContextPointer{
			Name: gateName + ".verdict", Integrity: verdictArtifact.Integrity, Artifact: verdictArtifact,
		}
		injected = &pointer
		pointers = append(pointers, pointer)
	}
	// #3843: a gate sending a stage BACK commits a learning episode
	// and hands the repass a pointer to it, so the next attempt argues
	// with the reviewer's finding instead of rediscovering it. Only on
	// a true re-entry — an advancing gate has nothing to teach.
	//
	// #3929: "sending BACK" is the gate result's own repass attempt,
	// the number resolveGateOutcome already charged to this target's
	// budget and r.retryDecision already journaled — not a second,
	// independently re-derived reading of the upstream map. The engine
	// used to ask `upstream[next] != nil` here (gateSendsBack), which
	// was equal to gr.Attempt >= 1 by construction at this call site
	// and so could drift from it without any test noticing.
	//
	// The injection is deliberately NOT conditioned on retryRoute.
	// That flag carries the retry CLASSIFIER's answer — automated
	// status-equals over nonzero_exit/base_sync_conflict, or `infra` —
	// which is a different question from whether a stage is being
	// asked to do its work again. An agentic reviewer resolving
	// needs-changes back into implement is the canonical repass and
	// the classifier declines it, so gating on retryRoute starved the
	// main lane's reviewer→implement loop of exactly the correction
	// internal/gate.reconcileLearningFindings exists to read back.
	// The retry-decision annotation above stays on retryRoute: only
	// the episode is widened.
	if runner.LearningEpisodeAppliesToBranch(learningEpisodeBranchFor(gr)) {
		episode, eerr := r.learningEpisode(ctx, in, gateName, next, gr, verdict, lastStage, lastResult, injected)
		if eerr != nil {
			return nil, fmt.Errorf("engine: journal learning episode for gate %q: %w", gateName, eerr)
		}
		if episode != nil {
			pointers = append(pointers, *episode)
		}
	}
	return pointers, nil
}
