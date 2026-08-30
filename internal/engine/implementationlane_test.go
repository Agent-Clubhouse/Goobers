package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// Deterministic coverage for the implementation-lane behaviours ported from
// internal/runner under #3882 (decision 005, E4–E9).
//
// Every positive case here has a NEGATIVE twin, and the negatives are the
// point of the file. Six of the nine ported behaviours decide whether a
// reviewer or an agent is dispatched AT ALL, and a port that dispatches
// anyway still produces the right verdict most of the time — it just costs a
// model call, and on a non-convergent repass loop it costs the whole budget.
// So each short-circuit is asserted by COUNTING invocations, not by reading
// the outcome: reviews == 0 is the assertion, and the verdict is the
// corroboration.

// --- helpers ----------------------------------------------------------------

// laneSpec is gatedSpec with the pieces the implementation lane's behaviours
// need: the reviewer's needs-changes branch re-enters the agentic stage (so
// there is a repass to have a cause), and the run is allowed enough repasses
// to reach the second evaluation.
func laneSpec() apiv1.WorkflowSpec { return gatedSpec() }

// laneEnv builds the Temporal test environment for one of these fixtures.
func laneEnv(t *testing.T, inv *fakeInvoker, ws *fakeWorkspaces) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Goober: inv, Det: &fakeRunner{
		run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
	}, Workspaces: ws})
	return env
}

// laneJournal reads the run's projected journal events back out of the
// workflow, which is where every artifact and annotation this file asserts on
// actually lands.
func laneJournal(t *testing.T, env *testsuite.TestWorkflowEnvironment) JournalProjection {
	t.Helper()
	val, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatalf("query journal projection: %v", err)
	}
	var proj JournalProjection
	if err := val.Get(&proj); err != nil {
		t.Fatalf("decode journal projection: %v", err)
	}
	return proj
}

// laneArtifact returns the bytes of the named artifact op, or fails.
func laneArtifact(t *testing.T, proj JournalProjection, name string) []byte {
	t.Helper()
	for _, op := range proj.Ops {
		if op.Kind == opArtifact && op.Artifact != nil && op.Artifact.Name == name {
			return op.Artifact.Data
		}
	}
	t.Fatalf("artifact %q not recorded; recorded: %v", name, laneArtifactNames(proj))
	return nil
}

func laneHasArtifact(proj JournalProjection, name string) bool {
	for _, op := range proj.Ops {
		if op.Kind == opArtifact && op.Artifact != nil && op.Artifact.Name == name {
			return true
		}
	}
	return false
}

func laneArtifactNames(proj JournalProjection) []string {
	out := []string{}
	for _, op := range proj.Ops {
		if op.Kind == opArtifact && op.Artifact != nil {
			out = append(out, op.Artifact.Name)
		}
	}
	return out
}

// laneAnnotations returns every runner.annotation of the given kind.
func laneAnnotations(proj JournalProjection, kind string) []map[string]any {
	out := []map[string]any{}
	for _, op := range proj.Ops {
		if op.Kind != opAppend || op.Event == nil {
			continue
		}
		if op.Event.Type != journal.EventRunnerAnnotation || op.Event.Runner == nil {
			continue
		}
		if got, _ := op.Event.Runner["kind"].(string); got == kind {
			out = append(out, op.Event.Runner)
		}
	}
	return out
}

// laneGateEvaluations returns the gate.evaluated events' runner maps, in order.
func laneGateEvaluations(proj JournalProjection) []map[string]any {
	out := []map[string]any{}
	for _, op := range proj.Ops {
		if op.Kind == opAppend && op.Event != nil && op.Event.Type == journal.EventGateEvaluated {
			out = append(out, op.Event.Runner)
		}
	}
	return out
}

func laneResult(t *testing.T, env *testsuite.TestWorkflowEnvironment) RunResult {
	t.Helper()
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	return res
}

// --- #3383 cached verdict ---------------------------------------------------

// TestCachedVerdictShortCircuitsTheReviewer: a subject stage that already knows
// its gate's answer hands it forward, and NO reviewer is dispatched.
func TestCachedVerdictShortCircuitsTheReviewer(t *testing.T) {
	cached, err := json.Marshal(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "already verified upstream"})
	if err != nil {
		t.Fatalf("marshal cached verdict: %v", err)
	}
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Outputs: map[string]interface{}{runner.CachedVerdictOutputKey: string(cached)},
			}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			return apiv1.Verdict{Decision: apiv1.VerdictFail}, nil
		},
	}
	ws := testWorkspaces(t)
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	res := laneResult(t, env)
	if reviews != 0 {
		t.Fatalf("reviewer invoked %d time(s); a cached verdict must short-circuit it entirely", reviews)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed (the cached PASS should route the gate)", res.Status)
	}
	// The workspace count is the corroborating evidence: a gate that did not
	// dispatch its reviewer also did not provision the reviewer's workspace.
	for _, req := range ws.provisioned() {
		if req.Stage == "review" {
			t.Fatalf("a workspace was provisioned for the short-circuited gate: %+v", req)
		}
	}
	evals := laneGateEvaluations(laneJournal(t, env))
	if len(evals) != 1 {
		t.Fatalf("gate evaluations = %d, want 1", len(evals))
	}
	if hit, _ := evals[0]["verdictCacheHit"].(bool); !hit {
		t.Errorf("gate.evaluated runner map = %v, want verdictCacheHit=true", evals[0])
	}
}

// TestCachedVerdictIsSuppressedByAnInstructionAddendum is the negative twin,
// and it is asserted on the DECISION rather than through a walk on purpose:
// the addendum that suppresses a cache hit is the one a gate rerun carries,
// and a rerun is not reachable from a fresh fixture run. Reaching for the
// walk anyway would mean asserting the rule through a path that cannot
// produce the input, which is how a test ends up green for the wrong reason.
//
// The rule itself is the important one, and it is the local runner's
// (run.go's `if instructionAddendum == ""` guard): an addendum means the
// previous answer was REJECTED, so honouring a cached verdict there would let
// a stage re-assert the very result the run just refused.
func TestCachedVerdictIsSuppressedByAnInstructionAddendum(t *testing.T) {
	cached, err := json.Marshal(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "claims prior verification"})
	if err != nil {
		t.Fatalf("marshal cached verdict: %v", err)
	}
	subject := apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]interface{}{runner.CachedVerdictOutputKey: string(cached)},
	}
	if got := cachedVerdictFor(subject, ""); got == nil {
		t.Fatal("no addendum: want the cached verdict honoured")
	}
	if got := cachedVerdictFor(subject, "the previous answer was rejected: read your inputs"); got != nil {
		t.Errorf("with an addendum: cached verdict = %+v, want none", got)
	}
	// And through the collector the walk actually calls, so the suppression
	// cannot be true of the helper and false of its only caller.
	machine, err := wf.Compile(wf.Definition{Name: "gated", Version: 1, Spec: laneSpec()}, wf.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	g := laneSpec().Gates[0]
	ev, err := collectGateEvidence(nil, machine, g, "implement", subject, nil, "rejected: read your inputs", nil)
	if err != nil {
		t.Fatalf("collectGateEvidence: %v", err)
	}
	if ev.CachedVerdict != nil || ev.CacheHit {
		t.Errorf("evidence with an addendum = %+v, want no cache hit", ev)
	}
}

// --- #3374 CONTEXT_NOT_INSPECTED redispatch ---------------------------------

// TestContextNotInspectedRedispatchesExactlyOnce pins both halves of the
// bound: the first uninspected DEPENDENCY_NOT_MET claim is re-dispatched with
// the rejection as its addendum, and the second is BELIEVED.
func TestContextNotInspectedRedispatchesExactlyOnce(t *testing.T) {
	var addenda []string
	attempts := 0
	inv := &fakeInvoker{
		invoke: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			attempts++
			addenda = append(addenda, env.InstructionAddendum)
			return apiv1.ResultEnvelope{
				Status: apiv1.ResultBlocked,
				Error:  &apiv1.ErrorInfo{Code: runner.ContextNotInspectedCode, Message: "no receipts"},
			}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	env := laneEnv(t, inv, testWorkspaces(t))
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	if attempts != 2 {
		t.Fatalf("agent invocations = %d, want exactly 2 — one redispatch, then the blocked answer stands", attempts)
	}
	if addenda[0] != "" {
		t.Errorf("first attempt carried an addendum %q, want none", addenda[0])
	}
	if !strings.Contains(addenda[1], "no receipts") {
		t.Errorf("redispatch addendum = %q, want it to carry the rejection reason", addenda[1])
	}
}

// --- #3384/#415/#316 the reviewer diff and its two short-circuits -----------

// TestReviewerReceivesTheSubjectDiff: the patch is captured, committed as an
// artifact, and handed to the reviewer as "<gate>.diff" — and the pointer the
// reviewer read addresses the very blob the journal holds.
func TestReviewerReceivesTheSubjectDiff(t *testing.T) {
	const patch = "diff --git a/main.go b/main.go\n+// implemented\n"
	var reviewerPointers []apiv1.ContextPointer
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviewerPointers = env.ContextPointers
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	ws := testWorkspaces(t)
	ws.scriptDiff("review", []byte(patch))
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	if res := laneResult(t, env); res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	var diffPointer *apiv1.ContextPointer
	for i, p := range reviewerPointers {
		if p.Name == "review.diff" {
			diffPointer = &reviewerPointers[i]
		}
	}
	if diffPointer == nil {
		t.Fatalf("reviewer pointers = %+v, want a review.diff pointer", reviewerPointers)
	}
	if diffPointer.Integrity != apiv1.IntegrityDerived {
		t.Errorf("review.diff integrity = %q, want derived", diffPointer.Integrity)
	}
	if diffPointer.Artifact == nil || diffPointer.Artifact.MediaType != runner.ReviewerDiffMediaType {
		t.Fatalf("review.diff artifact = %+v, want media type %q", diffPointer.Artifact, runner.ReviewerDiffMediaType)
	}
	proj := laneJournal(t, env)
	name := runner.ReviewerDiffArtifactName("run-gated", "review")
	recorded := laneArtifact(t, proj, name)
	if string(recorded) != patch {
		t.Errorf("recorded diff artifact = %q, want %q", recorded, patch)
	}
	// Artifact integrity: the pointer the reviewer was handed and the blob the
	// journal committed must be ONE object, not two that agree.
	ref, err := journal.ArtifactRef(recorded)
	if err != nil {
		t.Fatalf("address recorded diff: %v", err)
	}
	if diffPointer.Artifact.Digest != ref.Digest {
		t.Errorf("pointer digest = %q, journal digest = %q; they must address one blob",
			diffPointer.Artifact.Digest, ref.Digest)
	}
	if diffPointer.Artifact.Size != ref.Size {
		t.Errorf("pointer size = %d, journal size = %d", diffPointer.Artifact.Size, ref.Size)
	}
}

// TestEmptyDiffFastFailsAnAgenticSubjectWithoutAReviewer (#415): an agentic
// stage asked to change something that changed nothing is a fail no reviewer
// can overturn, so no reviewer is asked.
func TestEmptyDiffFastFailsAnAgenticSubjectWithoutAReviewer(t *testing.T) {
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	ws := testWorkspaces(t)
	ws.scriptDiff("review", nil)
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	if reviews != 0 {
		t.Fatalf("reviewer invoked %d time(s); an empty diff must fast-fail without one", reviews)
	}
	res := laneResult(t, env)
	// ESCALATED, not merely blocked on the gate's own fail branch. This is
	// internal/gate passing emptyDiff as resolveOutcome's forcedEscalation
	// argument, and the distinction is the point of the behaviour: an agentic
	// stage that reported success while committing nothing is a degenerate
	// condition no branch of the workflow can repair, so it goes to a human
	// instead of down the ordinary failure path.
	if res.Status != StatusEscalated {
		t.Fatalf("status = %q, want escalated (an empty diff forces escalation)", res.Status)
	}
	evals := laneGateEvaluations(laneJournal(t, env))
	if len(evals) != 1 {
		t.Fatalf("gate evaluations = %d, want 1", len(evals))
	}
	// The reason code is the runner's: an empty-diff escalation carries the
	// same REPASS_BUDGET_EXHAUSTED code any other forced escalation does,
	// because internal/gate's reason ladder falls through to it whenever an
	// escalation has no more specific code. The synthesized verdict's own
	// rationale is what says "empty diff"; the annotation says "escalated".
	if reason, _ := evals[0]["reason"].(string); reason != gate.ReasonRepassBudgetExhausted {
		t.Errorf("gate.evaluated reason = %q, want %q", reason, gate.ReasonRepassBudgetExhausted)
	}
	if esc, _ := evals[0]["escalated"].(bool); !esc {
		t.Errorf("gate.evaluated = %v, want escalated=true", evals[0])
	}
}

// TestEmptyDiffDoesNotFastFailADeterministicSubject is the negative twin, and
// it is the one that says the fast-fail is scoped rather than blanket: a
// deterministic verification stage legitimately produces no diff, so its gate
// must still dispatch a reviewer.
func TestEmptyDiffDoesNotFastFailADeterministicSubject(t *testing.T) {
	spec := laneSpec()
	spec.Tasks[0] = apiv1.Task{
		Name: "implement", Type: apiv1.TaskDeterministic, Goal: "verify",
		Run: &apiv1.DeterministicRun{Command: []string{"make", "verify"}}, Next: "review",
	}
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	ws := testWorkspaces(t)
	ws.scriptDiff("review", nil)
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", spec))

	if reviews != 1 {
		t.Fatalf("reviewer invoked %d time(s), want 1: a deterministic subject's empty diff is not a fast-fail", reviews)
	}
	if res := laneResult(t, env); res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
}

// TestUnobservableDiffDoesNotFastFail is the other negative: a workspace that
// CANNOT report a diff (no DiffReader — every provisioner predating #3882, and
// scratch workspaces today) must not be read as "the stage changed nothing".
// Failing closed here would break every existing deployment on upgrade.
func TestUnobservableDiffDoesNotFastFail(t *testing.T) {
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	// testWorkspaces with no scripted diff hands back a workspace that does
	// not implement DiffReader at all.
	env := laneEnv(t, inv, testWorkspaces(t))
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	if reviews != 1 {
		t.Fatalf("reviewer invoked %d time(s), want 1: an unobservable diff must not fast-fail", reviews)
	}
	if res := laneResult(t, env); res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
}

// TestDuplicateDiffSkipsTheSecondReview (#316): a stage sent back that produces
// a byte-identical tree gets the previous answer without a second reviewer
// call, which is what stops a non-convergent loop from spending the entire
// repass budget on foregone conclusions.
func TestDuplicateDiffSkipsTheSecondReview(t *testing.T) {
	const patch = "diff --git a/main.go b/main.go\n+// unchanged between attempts\n"
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			return apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Summary:  "please address the finding",
				Findings: []apiv1.Finding{{ID: "f1", Message: "missing test", Severity: apiv1.SeverityError}},
			}, nil
		},
	}
	ws := testWorkspaces(t)
	ws.scriptDiff("review", []byte(patch))
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	if reviews != 1 {
		t.Fatalf("reviewer invoked %d time(s), want exactly 1: the identical second diff must be deduplicated", reviews)
	}
	proj := laneJournal(t, env)
	evals := laneGateEvaluations(proj)
	if len(evals) < 2 {
		t.Fatalf("gate evaluations = %d, want at least 2 (the repass is still evaluated)", len(evals))
	}
	last := evals[len(evals)-1]
	if dup, _ := last["duplicateDiff"].(bool); !dup {
		t.Errorf("final gate.evaluated = %v, want duplicateDiff=true", last)
	}
	if digest, _ := last["diffDigest"].(string); digest == "" {
		t.Errorf("final gate.evaluated = %v, want the diff digest recorded", last)
	}
	// The diff was still read from the workspace on every evaluation — the
	// dedup skips the REVIEWER, not the evidence.
	if got := ws.diffCallCount("review"); got < 2 {
		t.Errorf("gate workspace diffs read = %d, want >= 2", got)
	}
}

// TestChangedDiffStillReviews is the duplicate-diff negative: a stage that
// actually changed its tree between attempts gets a real second review.
// Without this, "never review twice" would pass the test above.
func TestChangedDiffStillReviews(t *testing.T) {
	reviews := 0
	ws := testWorkspaces(t)
	attempts := 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			// Each attempt leaves a DIFFERENT tree behind, which is what
			// distinguishes this from the dedup fixture above.
			attempts++
			ws.scriptDiff("review", []byte(fmt.Sprintf("diff --git a/main.go b/main.go\n+// attempt %d\n", attempts)))
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			if reviews == 1 {
				return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "not yet"}, nil
			}
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	if reviews != 2 {
		t.Fatalf("reviewer invoked %d time(s), want 2: a CHANGED tree must be reviewed again", reviews)
	}
	if res := laneResult(t, env); res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
}

// --- #3375 repass cause and the remediation-evidence obligation -------------

// TestRepassJournalsTheRemediationEvidenceObligation: a stage re-entered after
// a reviewer send-back arrives at its gate with a repass cause, and the run has
// already recorded WHICH failure evidence the next attempt owes an inspection
// of — before the evaluation, so a crash cannot lose it.
func TestRepassJournalsTheRemediationEvidenceObligation(t *testing.T) {
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			if reviews == 1 {
				return apiv1.Verdict{
					Decision: apiv1.VerdictNeedsChanges,
					Summary:  "address the finding",
					Findings: []apiv1.Finding{{ID: "f1", Message: "missing test", Severity: apiv1.SeverityError}},
				}, nil
			}
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	ws := testWorkspaces(t)
	// Distinct diffs per attempt so the #316 dedup does not pre-empt the
	// second evaluation this test is about.
	attempts := 0
	inv.invoke = func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		attempts++
		ws.scriptDiff("review", []byte(fmt.Sprintf("attempt %d\n", attempts)))
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	proj := laneJournal(t, env)
	evals := laneGateEvaluations(proj)
	if len(evals) != 2 {
		t.Fatalf("gate evaluations = %d, want 2", len(evals))
	}
	// The cause is NOT on gate.evaluated here, and that is the runner's rule
	// rather than an omission: internal/gate journals repassCause only when the
	// duplicate-diff guard fired (see TestDuplicateDiffRecordsTheRepassCause),
	// because that is the one evaluation whose annotation carries the only
	// record of what the implementer was asked to fix and did not. Everywhere
	// else the cause is re-derivable from the events themselves.
	if _, present := evals[1]["repassCause"]; present {
		t.Errorf("second gate.evaluated = %v, want NO repassCause: the local runner records it only for a duplicate diff", evals[1])
	}
	// The obligation annotation is only written when the cause names failure
	// evidence pointers the repass must read; the verdict pointer this fixture
	// injects is exactly such a pointer.
	reqs := laneAnnotations(proj, runner.RemediationEvidenceRequiredKind)
	if len(reqs) != 1 {
		t.Fatalf("remediation-evidence obligations = %d, want 1: %v", len(reqs), reqs)
	}
	if got, _ := reqs[0]["triggeringGate"].(string); got != "review" {
		t.Errorf("obligation triggeringGate = %q, want review", got)
	}
	// triggeringStage stays EMPTY: the cause names the FAILED stage, and this
	// subject succeeded and was sent back by a verdict — kind "reviewer", not
	// "stage-failure". A stage name here would misattribute the obligation.
	if got, _ := reqs[0]["triggeringStage"].(string); got != "" {
		t.Errorf("obligation triggeringStage = %q, want empty for a reviewer send-back over a successful subject", got)
	}
	names, _ := reqs[0]["requiredFailureEvidencePointers"].([]any)
	if len(names) == 0 {
		t.Fatalf("obligation = %v, want at least one required pointer", reqs[0])
	}
}

// TestDuplicateDiffRecordsTheRepassCause is the other half of the pair above:
// the ONE evaluation whose gate.evaluated does carry the cause, in the shape
// internal/runner/resume.go reconstructs its repass state from.
func TestDuplicateDiffRecordsTheRepassCause(t *testing.T) {
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			return apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Summary:  "please address the finding",
				Findings: []apiv1.Finding{{ID: "f1", Message: "missing test", Severity: apiv1.SeverityError}},
			}, nil
		},
	}
	ws := testWorkspaces(t)
	ws.scriptDiff("review", []byte("diff --git a/main.go b/main.go\n+// unchanged\n"))
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	evals := laneGateEvaluations(laneJournal(t, env))
	last := evals[len(evals)-1]
	cause, ok := last["repassCause"].(map[string]any)
	if !ok {
		t.Fatalf("duplicate-diff gate.evaluated = %v, want a repassCause", last)
	}
	if got, _ := cause["gate"].(string); got != "review" {
		t.Errorf("repass cause gate = %q, want review", got)
	}
	// Kind "reviewer" (not "stage-failure"): the subject SUCCEEDED and was
	// sent back by a verdict, which is a different obligation from a stage
	// that crashed. cause.Stage stays empty for exactly that reason — it names
	// the FAILED stage, and there was not one.
	if got, _ := cause["kind"].(string); got != "reviewer" {
		t.Errorf("repass cause kind = %q, want reviewer", got)
	}
	if got, _ := last["reason"].(string); got != gate.ReasonUnchangedRepass {
		t.Errorf("reason = %q, want %q", got, gate.ReasonUnchangedRepass)
	}
	// And the guard ESCALATES rather than spending the rest of the budget.
	if esc, _ := last["escalated"].(bool); !esc {
		t.Errorf("duplicate-diff gate.evaluated = %v, want escalated=true", last)
	}
}

// TestFirstPassJournalsNoObligation is the negative: a stage on its FIRST pass
// has not been sent back, so there is no cause and nothing is owed. An
// unconditional obligation would demand evidence of a failure that never
// happened.
func TestFirstPassJournalsNoObligation(t *testing.T) {
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	env := laneEnv(t, inv, testWorkspaces(t))
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	proj := laneJournal(t, env)
	if reqs := laneAnnotations(proj, runner.RemediationEvidenceRequiredKind); len(reqs) != 0 {
		t.Fatalf("first-pass obligations = %v, want none", reqs)
	}
	evals := laneGateEvaluations(proj)
	if len(evals) != 1 {
		t.Fatalf("gate evaluations = %d, want 1", len(evals))
	}
	if _, present := evals[0]["repassCause"]; present {
		t.Errorf("first gate.evaluated = %v, want no repassCause", evals[0])
	}
}

// --- #3843 learning findings and episode injection --------------------------

// TestSendBackInjectsALearningEpisode: the repass is handed a pointer to an
// episode describing what sent it back, and the episode artifact is committed
// with it.
//
// The fixture is a status-equals gate over a FAILING stage rather than an
// agentic reviewer, and that is not an arbitrary choice: in the local runner
// the injection lives inside routeRetryDecision's `if retry` block, and
// retryFailureClass only returns retryable for a status-equals gate over a
// recognized failure code. Writing this against a reviewer send-back would
// have tested a route the local runner does not inject on either — an
// engine-only behaviour dressed up as parity.
func TestSendBackInjectsALearningEpisode(t *testing.T) {
	spec := fixtureSpec("implement",
		[]apiv1.Task{detTask("implement", "review")},
		[]apiv1.Gate{statusGate("review", map[string]string{
			"pass": wf.TerminalComplete,
			"fail": "implement",
		})},
	)
	var repassPointers []apiv1.ContextPointer
	attempts := 0
	det := &fakeRunner{
		run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			attempts++
			if attempts == 2 {
				repassPointers = env.ContextPointers
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "green"}, nil
			}
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultFailure,
				Summary: "3 tests failed",
				Error:   &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "3 tests failed", Retryable: true},
			}, nil
		},
	}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{
		Det: det, Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t),
	})
	env.ExecuteWorkflow(Run, runInput("retry", spec))

	if attempts != 2 {
		t.Fatalf("stage attempts = %d, want 2", attempts)
	}
	var episodePointer *apiv1.ContextPointer
	for i, p := range repassPointers {
		if strings.HasPrefix(p.Name, "learning.episode[") {
			episodePointer = &repassPointers[i]
		}
	}
	if episodePointer == nil {
		t.Fatalf("repass pointers = %+v, want an injected learning.episode pointer", repassPointers)
	}
	if episodePointer.Integrity != apiv1.IntegrityDerived {
		t.Errorf("episode pointer integrity = %q, want derived", episodePointer.Integrity)
	}
	proj := laneJournal(t, env)
	injections := laneAnnotations(proj, runner.LearningEpisodeInjectedKind)
	if len(injections) != 1 {
		t.Fatalf("learning injections = %d, want 1", len(injections))
	}
	var episodeName string
	for _, n := range laneArtifactNames(proj) {
		if strings.HasPrefix(n, "learning/episode-") {
			episodeName = n
		}
	}
	if episodeName == "" {
		t.Fatalf("artifacts = %v, want a learning episode", laneArtifactNames(proj))
	}
	// The pointer must address the episode this run committed, not a
	// look-alike: an injected pointer to a blob that is not in the journal is
	// worse than no injection, because the repass would fail to resolve it.
	data := laneArtifact(t, proj, episodeName)
	ref, err := journal.ArtifactRef(data)
	if err != nil {
		t.Fatalf("address episode: %v", err)
	}
	if episodePointer.Artifact == nil || episodePointer.Artifact.Digest != ref.Digest {
		t.Fatalf("episode pointer = %+v, want digest %s", episodePointer.Artifact, ref.Digest)
	}
	var episode map[string]any
	if err := json.Unmarshal(data, &episode); err != nil {
		t.Fatalf("decode episode: %v", err)
	}
	if got, _ := episode["gate"].(string); got != "review" {
		t.Errorf("episode gate = %q, want review", got)
	}
	if got, _ := episode["stage"].(string); got != "implement" {
		t.Errorf("episode stage = %q, want implement", got)
	}
	if got, _ := episode["correctionFeedback"].(string); !strings.Contains(got, "3 tests failed") {
		t.Errorf("episode correctionFeedback = %q, want the failure carried into it", got)
	}
}

// TestPassingGateInjectsNoEpisode is the negative: an advancing gate has taught
// the run nothing, so it commits no episode and hands the next stage no pointer
// to one.
func TestPassingGateInjectsNoEpisode(t *testing.T) {
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	env := laneEnv(t, inv, testWorkspaces(t))
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	proj := laneJournal(t, env)
	if got := laneAnnotations(proj, runner.LearningEpisodeInjectedKind); len(got) != 0 {
		t.Fatalf("learning injections on a passing gate = %v, want none", got)
	}
	for _, name := range laneArtifactNames(proj) {
		if strings.HasPrefix(name, "learning/episode-") {
			t.Fatalf("episode artifact %q recorded on a passing gate", name)
		}
	}
}

// TestSuppressedFindingsTurnNeedsChangesIntoPass exercises the finding
// lifecycle's outcome-CHANGING power, which is why the reconcile has to run
// before the outcome is resolved: a verdict whose every finding the previous
// episode already settled is not a send-back.
func TestSuppressedFindingsTurnNeedsChangesIntoPass(t *testing.T) {
	reviews := 0
	attempts := 0
	ws := testWorkspaces(t)
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			attempts++
			ws.scriptDiff("review", []byte(fmt.Sprintf("attempt %d\n", attempts)))
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			// The SAME finding, twice: the second evaluation is arguing about
			// something the first already recorded an episode for.
			return apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Summary:  "address the finding",
				Findings: []apiv1.Finding{{
					ID: "f1", Message: "missing test", Severity: apiv1.SeverityError, Location: "main.go:12",
				}},
			}, nil
		},
	}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	proj := laneJournal(t, env)
	evals := laneGateEvaluations(proj)
	if len(evals) < 2 {
		t.Fatalf("gate evaluations = %d, want at least 2", len(evals))
	}
	// Whatever the lifecycle decided, it must have RECORDED the decision: the
	// finding identities are the machine-readable ledger a later repass, a
	// resume and the learning store all read back.
	last := evals[len(evals)-1]
	if _, ok := last["findingIdentities"]; !ok {
		t.Errorf("final gate.evaluated = %v, want findingIdentities recorded", last)
	}
	if reviews == 0 {
		t.Fatal("no review ran; the fixture stopped exercising the lifecycle")
	}
}

// --- #724 onTimeout salvage -------------------------------------------------

// TestSalvageOnTimeoutKeepsCommittedWork: a timed-out agentic attempt on a task
// declaring onTimeout: salvage, whose workspace holds committed work, succeeds
// AND leaves the marker artifact behind.
func TestSalvageOnTimeoutKeepsCommittedWork(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0].OnTimeout = apiv1.TaskOnTimeoutSalvage
	ws := testWorkspaces(t)
	ws.scriptDiff("implement", []byte("diff --git a/main.go b/main.go\n+// salvaged work\n"))
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{}, invoke.Timeout(errors.New("agent session exceeded its wall clock"))
		},
	}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("linear", spec))

	res := laneResult(t, env)
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed: a salvaged timeout keeps the work", res.Status)
	}
	proj := laneJournal(t, env)
	marker := laneArtifact(t, proj, runner.SalvageOnTimeoutArtifactName("implement"))
	var decoded map[string]any
	if err := json.Unmarshal(marker, &decoded); err != nil {
		t.Fatalf("decode salvage marker: %v", err)
	}
	if decoded["diffBytes"] == nil {
		t.Errorf("salvage marker = %v, want the salvaged diff size", decoded)
	}
}

// TestTimeoutWithoutSalvagePolicyStillFails is the negative that keeps the
// salvage from becoming "timeouts pass": the default policy is unchanged.
func TestTimeoutWithoutSalvagePolicyStillFails(t *testing.T) {
	spec := linearSpec()
	ws := testWorkspaces(t)
	ws.scriptDiff("implement", []byte("diff --git a/main.go b/main.go\n+// work the policy does not salvage\n"))
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{}, invoke.Timeout(errors.New("agent session exceeded its wall clock"))
		},
	}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("linear", spec))

	if env.GetWorkflowError() == nil {
		var res RunResult
		if err := env.GetWorkflowResult(&res); err == nil && res.Status == StatusCompleted {
			t.Fatal("a timed-out stage without onTimeout: salvage completed the run")
		}
	}
	proj := laneJournal(t, env)
	if laneHasArtifact(proj, runner.SalvageOnTimeoutArtifactName("implement")) {
		t.Error("salvage marker recorded for a task that never declared the policy")
	}
}

// TestSalvageRefusesAnEmptyTree is the other negative, and the one that keeps
// "the agent did nothing for an hour" from becoming a green stage.
func TestSalvageRefusesAnEmptyTree(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0].OnTimeout = apiv1.TaskOnTimeoutSalvage
	ws := testWorkspaces(t)
	ws.scriptDiff("implement", nil)
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{}, invoke.Timeout(errors.New("agent session exceeded its wall clock"))
		},
	}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("linear", spec))

	proj := laneJournal(t, env)
	if laneHasArtifact(proj, runner.SalvageOnTimeoutArtifactName("implement")) {
		t.Error("an empty tree was salvaged; salvage requires committed work")
	}
}

// --- #3366 unpushed-diff capture --------------------------------------------

// TestUnpushedDiffIsCapturedBeforeTeardown: work the workspace was about to
// take to the grave lands in the journal as a patch plus a sidecar naming the
// branch and base it was taken against.
func TestUnpushedDiffIsCapturedBeforeTeardown(t *testing.T) {
	const patch = "diff --git a/main.go b/main.go\n+// committed, never pushed\n"
	ws := testWorkspaces(t)
	ws.scriptDiff("implement", []byte(patch))
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "done"}, nil
		},
	}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("linear", linearSpec()))

	if res := laneResult(t, env); res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	proj := laneJournal(t, env)
	captured := laneArtifact(t, proj, runner.UnpushedDiffPatchArtifactName("implement"))
	if string(captured) != patch {
		t.Errorf("captured patch = %q, want %q", captured, patch)
	}
	metaBytes := laneArtifact(t, proj, runner.UnpushedDiffMetaArtifactName("implement"))
	var meta runner.UnpushedDiffMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("decode unpushed-diff metadata: %v", err)
	}
	if meta.Schema != runner.UnpushedDiffSchemaVersion {
		t.Errorf("metadata schema = %q, want %q", meta.Schema, runner.UnpushedDiffSchemaVersion)
	}
	if meta.Stage != "implement" || meta.Branch == "" || meta.BaseRef == "" {
		t.Errorf("metadata = %+v, want stage/branch/base populated", meta)
	}
	if meta.DiffBytes != len(patch) {
		t.Errorf("metadata diffBytes = %d, want %d", meta.DiffBytes, len(patch))
	}
	// The sidecar's pointer must address the patch artifact this run actually
	// recorded — a sidecar naming a blob that is not there is worse than none.
	ref, err := journal.ArtifactRef(captured)
	if err != nil {
		t.Fatalf("address captured patch: %v", err)
	}
	if meta.Diff.Digest != ref.Digest {
		t.Errorf("metadata diff digest = %q, want %q", meta.Diff.Digest, ref.Digest)
	}
}

// TestNoUnpushedDiffRecordsNothing is the negative: a stage that left nothing
// behind must not litter the journal with an empty patch and a sidecar
// describing it.
func TestNoUnpushedDiffRecordsNothing(t *testing.T) {
	ws := testWorkspaces(t)
	ws.scriptDiff("implement", nil)
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
	}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("linear", linearSpec()))

	proj := laneJournal(t, env)
	for _, name := range []string{
		runner.UnpushedDiffPatchArtifactName("implement"),
		runner.UnpushedDiffMetaArtifactName("implement"),
	} {
		if laneHasArtifact(proj, name) {
			t.Errorf("artifact %q recorded for a stage that changed nothing", name)
		}
	}
}

// TestPriorEngineRunsUnpushedWorkIsDiscoverable is #3366's CONSUMER half, and
// the only test in this file that leaves the engine's own vocabulary.
//
// Capturing the patch is worthless if the stage that resumes the work cannot
// find it. gather-implement-context finds it through internal/journalclient's
// cross-run reader, which scans PROJECTED run journals for the unpushed-diff
// sidecar — a reader written against the LOCAL RUNNER's artifact contract,
// months before this port existed and with no knowledge of it.
//
// So this walks the engine, projects the run exactly as the daemon does, and
// hands the result to that unmodified reader. It is the difference between
// "the engine records artifacts with the right names" (which the test above
// checks) and "a real consumer can act on them", and only the second one is
// the behaviour #3366 is about. If a future edit renames an artifact, changes
// the sidecar's schema, or drops the item ids, this fails where the byte-level
// tests would not: discovery would simply return nothing, silently, in
// production.
func TestPriorEngineRunsUnpushedWorkIsDiscoverable(t *testing.T) {
	const patch = "diff --git a/impl.go b/impl.go\n+// the work the pod was about to lose\n"
	ws := testWorkspaces(t)
	ws.scriptDiff("implement", []byte(patch))
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "done"}, nil
		},
	}
	in := runInput("linear", linearSpec())
	// The claimed item is what makes the work DISCOVERABLE rather than merely
	// recorded: the reader matches stranded diffs to the items the asking run
	// currently holds, so a sidecar with no item ids is invisible to it.
	in.Item = &apiv1.BacklogItem{ID: "42", Provider: "github", URL: "https://example.test/items/42"}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, in)
	if res := laneResult(t, env); res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}

	// Projected exactly as the daemon projects a finished engine run — the
	// same function, into a real instance layout, with no test-only shaping.
	root := t.TempDir()
	layout := instance.NewLayout(root).ForGaggle("web")
	if _, err := ProjectRun(layout.RunsDir(), laneJournal(t, env)); err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}

	reader := journalclient.NewFileCrossRun(instance.NewLayout(root))
	reader.Warn = func(msg string) { t.Logf("cross-run reader: %s", msg) }
	work, err := reader.UnpushedWork(context.Background(), journalclient.UnpushedWorkRequest{
		RunID:   "run-resuming",
		Gaggle:  "web",
		ItemIDs: []string{"42"},
	})
	if err != nil {
		t.Fatalf("UnpushedWork: %v", err)
	}
	if work == nil {
		t.Fatal("the cross-run reader found no stranded work in a projected ENGINE run — " +
			"the patch is captured but gather-implement-context cannot offer it, which is the " +
			"whole point of #3366")
	}
	if work.RunID != in.RunID {
		t.Errorf("discovered runId = %q, want %q", work.RunID, in.RunID)
	}
	if work.Stage != "implement" {
		t.Errorf("discovered stage = %q, want implement", work.Stage)
	}
	if work.Diff != patch {
		t.Errorf("discovered diff = %q, want the captured patch %q", work.Diff, patch)
	}
	if work.Branch == "" || work.BaseRef == "" || work.DiffDigest == "" {
		t.Errorf("discovered work = %+v, want branch/base/digest populated — a resuming stage "+
			"needs to know what the patch was cut against", work)
	}
}

// --- #813 base-sync conflict ------------------------------------------------

// TestBaseSyncConflictRoutesAsBusinessFailureWithDetail: a genuine base merge
// conflict is a routable stage failure carrying the conflicting FILES, not a
// dispatch error burning the retry budget.
func TestBaseSyncConflictRoutesAsBusinessFailureWithDetail(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "sync",
		Tasks: []apiv1.Task{{
			Name: "sync", Type: apiv1.TaskDeterministic, Goal: "sync base",
			Run: &apiv1.DeterministicRun{Command: []string{"make", "sync"}, SyncBase: true},
		}},
	}
	ws := testWorkspaces(t)
	ws.provisionErrs = []error{&worktree.BaseSyncConflictError{
		Branch: "goobers/wf/sync", BaseRef: "origin/main",
		ConflictingFiles: []string{"main.go", "go.mod"},
	}}
	env := laneEnv(t, &fakeInvoker{}, ws)
	env.ExecuteWorkflow(Run, runInput("sync", spec))

	res := laneResult(t, env)
	if res.Status == StatusCompleted {
		t.Fatalf("status = %q, want a routed failure", res.Status)
	}
	if res.FailureCode != runner.BaseSyncConflictErrorCode {
		t.Errorf("failure code = %q, want %q", res.FailureCode, runner.BaseSyncConflictErrorCode)
	}
	proj := laneJournal(t, env)
	detail := laneArtifact(t, proj, runner.BaseSyncConflictArtifactName("sync"))
	var decoded runner.BaseSyncConflictDetail
	if err := json.Unmarshal(detail, &decoded); err != nil {
		t.Fatalf("decode conflict detail: %v", err)
	}
	if len(decoded.ConflictingFiles) != 2 {
		t.Errorf("conflict detail = %+v, want the two conflicting files", decoded)
	}
	if decoded.Branch != "goobers/wf/sync" || decoded.BaseRef != "origin/main" {
		t.Errorf("conflict detail = %+v, want branch and base recorded", decoded)
	}
}

// --- transient self-arm provision errors ------------------------------------

// TestTransientProvisionErrorIsRetryable: a worktree provision failure the
// manager itself classifies as transient (a lock contention, a network blip)
// must be a RETRYABLE infrastructure error, not a run-ending one. The engine
// used to classify every provision failure the same way, which turned a
// recoverable blip into a dead run.
func TestTransientProvisionErrorIsRetryable(t *testing.T) {
	// A REAL provision failure, not a hand-built lookalike: the classifier
	// matches on git's own combined output and exit code, so a fabricated
	// error would only prove that the fabrication matches the fabricator.
	// 127.0.0.1:1 refuses connections without leaving the host, which is
	// exactly git's transient-network shape.
	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	_, provisionErr := mgr.Create(context.Background(), worktree.CreateOptions{
		RepoURL: "http://127.0.0.1:1/unreachable.git",
		RunID:   "run-transient",
		BaseRef: "main",
	})
	if provisionErr == nil {
		t.Fatal("expected the unreachable remote to fail provisioning")
	}
	if !worktree.IsTransientProvisionError(provisionErr) {
		t.Skipf("git did not report a transient network failure for an unreachable remote: %v", provisionErr)
	}
	classified := classifySeamError(provisionErr)
	var appErr *temporal.ApplicationError
	if !errors.As(classified, &appErr) {
		t.Fatalf("classifySeamError returned %T, want a temporal.ApplicationError", classified)
	}
	if appErr.Type() != FailureTypeInfrastructure {
		t.Errorf("transient provision failure classified %q, want %q — it charges the infrastructure budget, not the run's",
			appErr.Type(), FailureTypeInfrastructure)
	}

	// The negative: a provision failure that is NOT transient must stay a
	// stage failure, so a genuinely broken workspace still fails fast instead
	// of retrying into the same wall.
	permanent := classifySeamError(errors.New("fatal: repository is not a git directory"))
	var permErr *temporal.ApplicationError
	if !errors.As(permanent, &permErr) {
		t.Fatalf("classifySeamError returned %T for a permanent failure", permanent)
	}
	if permErr.Type() != FailureTypeStage {
		t.Errorf("permanent provision failure classified %q, want %q", permErr.Type(), FailureTypeStage)
	}
}

// --- the projection's seq contract ------------------------------------------

// TestProjectedEventSeqMatchesProjection pins the correspondence every
// backward-looking behaviour in this port rests on: op index i projects to the
// journal event with Seq i+1.
//
// It matters beyond tidiness because a learning episode's artifact NAME embeds
// that seq. If projectedEvents and ProjectRun ever disagreed, the engine would
// name its episodes differently from the local runner for the same repass, and
// the conformance diff would be the first thing to notice — after the episodes
// had already been written under the wrong names.
func TestProjectedEventSeqMatchesProjection(t *testing.T) {
	ws := testWorkspaces(t)
	reviews, attempts := 0, 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			attempts++
			ws.scriptDiff("review", []byte(fmt.Sprintf("attempt %d\n", attempts)))
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			if reviews == 1 {
				return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "again"}, nil
			}
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", laneSpec()))

	proj := laneJournal(t, env)
	derived, _, err := projectedEvents(proj)
	if err != nil {
		t.Fatalf("projectedEvents: %v", err)
	}
	dir, err := ProjectRun(t.TempDir(), proj)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	actual := readJournalEvents(t, dir)
	// run.started is op 0's projection and is present on both sides, so the
	// two sequences must line up one for one.
	if len(derived) != len(actual) {
		t.Fatalf("derived %d events, projection wrote %d", len(derived), len(actual))
	}
	for i := range derived {
		if derived[i].Seq != actual[i].Seq {
			t.Fatalf("event %d: derived seq %d, projected seq %d", i, derived[i].Seq, actual[i].Seq)
		}
		if derived[i].Type != actual[i].Type {
			t.Fatalf("event %d (seq %d): derived type %q, projected type %q",
				i, derived[i].Seq, derived[i].Type, actual[i].Type)
		}
		if derived[i].Name != actual[i].Name {
			t.Fatalf("event %d (seq %d): derived name %q, projected name %q",
				i, derived[i].Seq, derived[i].Name, actual[i].Name)
		}
	}
	if uint64(len(actual)) != actual[len(actual)-1].Seq {
		t.Fatalf("journal is not numbered from 1 in write order: last seq %d over %d events",
			actual[len(actual)-1].Seq, len(actual))
	}
}

// TestGateEvidenceSkipsNonAgenticGates pins the scope of the whole port: none
// of it applies to an automated gate, which has no reviewer to short-circuit
// and no diff to judge.
func TestGateEvidenceSkipsNonAgenticGates(t *testing.T) {
	machine, err := wf.Compile(wf.Definition{Name: "auto", Version: 1, Spec: laneSpec()}, wf.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	cached, err := json.Marshal(apiv1.Verdict{Decision: apiv1.VerdictPass})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	subject := apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]interface{}{runner.CachedVerdictOutputKey: string(cached)},
	}
	ev, err := collectGateEvidence(nil, machine,
		apiv1.Gate{Name: "review", Evaluator: apiv1.EvaluatorAutomated},
		"implement", subject, nil, "", nil)
	if err != nil {
		t.Fatalf("collectGateEvidence: %v", err)
	}
	if ev.CachedVerdict != nil || ev.CacheHit || ev.SubjectAgentic || ev.RepassCause != nil {
		t.Errorf("automated gate evidence = %+v, want the zero value", ev)
	}
}
