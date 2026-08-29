package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// flakyReviewer is a reviewer Goober that returns a transient error for its
// first `failures` calls, then the given verdict — the shape of the #765 live
// symptom (a copilot session that intermittently writes no verdict file).
type flakyReviewer struct {
	failures int
	err      error
	verdict  apiv1.Verdict
	calls    int
}

type correctingReviewer struct {
	invalid bool
	calls   int
	addenda []string
}

func (r *correctingReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, nil
}

func (r *correctingReviewer) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.calls++
	r.addenda = append(r.addenda, env.InstructionAddendum)
	if r.invalid {
		return apiv1.Verdict{Decision: apiv1.VerdictFail, Rationale: "The approach needs a policy decision."}, nil
	}
	if r.calls == 1 {
		return apiv1.Verdict{Decision: apiv1.VerdictFail, Rationale: "The approach needs a policy decision."}, nil
	}
	return apiv1.Verdict{Decision: apiv1.VerdictFail, Rationale: "Should this policy be adopted?"}, nil
}

func (f *flakyReviewer) Invoke(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, nil
}

func (f *flakyReviewer) Review(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	f.calls++
	if f.calls <= f.failures {
		return apiv1.Verdict{}, f.err
	}
	return f.verdict, nil
}

func retryGate(policy *apiv1.RetryPolicy) apiv1.Gate {
	return apiv1.Gate{
		Name:      "review",
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "rev", Retry: policy},
		Branches:  map[string]string{OutcomePass: "done", OutcomeFail: "implement"},
	}
}

// noCompletionErr mimics the wrapped harness error the reviewer surfaces when a
// session writes no verdict file (the exact #765 symptom string), so
// errors.Is(err, harness.ErrNoCompletion) holds and reviewer.go tags it
// transient.
func noCompletionErr() error {
	return fmt.Errorf("harness: copilot-cli: %w: .goobers/verdict.json", harness.ErrNoCompletion)
}

func readErrorEvents(t *testing.T, run *journal.Run) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(run.Dir())
	if err != nil {
		t.Fatalf("journal.OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var errs []journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventError {
			errs = append(errs, ev)
		}
	}
	return errs
}

// TestEvaluateRetriesTransientReviewerErrorThenSucceeds (#765 AC 1+2): a
// transient reviewer-harness error retries within the declared bound and the
// gate then evaluates normally on the successful attempt, with the failed
// attempt journaled.
func TestEvaluateRetriesTransientReviewerErrorThenSucceeds(t *testing.T) {
	rev := &flakyReviewer{failures: 1, err: noCompletionErr(), verdict: apiv1.Verdict{Decision: apiv1.VerdictPass}}
	run := newTestJournal(t)
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}, Journal: run}

	r, err := ev.Evaluate(context.Background(), retryGate(&apiv1.RetryPolicy{MaxAttempts: 2}),
		apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate returned error, want retry-then-succeed: %v", err)
	}
	if rev.calls != 2 {
		t.Errorf("reviewer called %d times, want 2 (1 transient failure + 1 success)", rev.calls)
	}
	if r.Outcome != OutcomePass {
		t.Errorf("outcome = %q, want %q", r.Outcome, OutcomePass)
	}

	errEvents := readErrorEvents(t, run)
	if len(errEvents) != 1 {
		t.Fatalf("journaled %d error events, want 1 for the retried attempt", len(errEvents))
	}
	if errEvents[0].Error == nil || errEvents[0].Error.Code != "evaluator_transient" {
		t.Errorf("retry event error = %+v, want code evaluator_transient", errEvents[0].Error)
	}
	// Runner annotations round-trip through JSON, so the attempt number reads
	// back as a float64.
	if got := errEvents[0].Runner["evaluatorAttempt"]; got != float64(1) {
		t.Errorf("retry event evaluatorAttempt = %v (%T), want 1", got, got)
	}
}

// TestEvaluateRetryExhaustedFailsRun (#765 AC 3): a persistently transient error
// still fails the run once the declared bound is exhausted — no silent infinite
// retry — and every attempt is journaled.
func TestEvaluateRetryExhaustedFailsRun(t *testing.T) {
	rev := &flakyReviewer{failures: 99, err: noCompletionErr()}
	run := newTestJournal(t)
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}, Journal: run}

	_, err := ev.Evaluate(context.Background(), retryGate(&apiv1.RetryPolicy{MaxAttempts: 3}),
		apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err == nil {
		t.Fatal("Evaluate returned nil, want run-fatal error after exhausting the retry bound")
	}
	if !errors.Is(err, harness.ErrNoCompletion) {
		t.Errorf("error = %v, want it to wrap harness.ErrNoCompletion", err)
	}
	if rev.calls != 3 {
		t.Errorf("reviewer called %d times, want 3 (the declared MaxAttempts)", rev.calls)
	}
	if got := len(readErrorEvents(t, run)); got != 3 {
		t.Errorf("journaled %d error events, want 3 (one per attempt)", got)
	}
}

// TestEvaluateNonTransientErrorFailsFast (#765 AC 4): a non-transient evaluator
// error is NOT retried even when the gate declares a RetryPolicy — no wasted
// attempts.
func TestEvaluateNonTransientErrorFailsFast(t *testing.T) {
	rev := &flakyReviewer{failures: 99, err: errors.New("malformed reviewer config")}
	run := newTestJournal(t)
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}, Journal: run}

	_, err := ev.Evaluate(context.Background(), retryGate(&apiv1.RetryPolicy{MaxAttempts: 5}),
		apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err == nil {
		t.Fatal("Evaluate returned nil, want fail-fast on a non-transient error")
	}
	if rev.calls != 1 {
		t.Errorf("reviewer called %d times, want 1 (non-transient errors never retry)", rev.calls)
	}
	if got := len(readErrorEvents(t, run)); got != 0 {
		t.Errorf("journaled %d retry error events, want 0 (a non-transient error is not a retry)", got)
	}
}

// TestEvaluateNoRetryPolicyFailsFast is the blast-radius guard: a gate that
// declares no RetryPolicy keeps the pre-#765 behavior — even a transient error
// fails the run on the first attempt, so unmodified gates are unaffected.
func TestEvaluateNoRetryPolicyFailsFast(t *testing.T) {
	rev := &flakyReviewer{failures: 99, err: noCompletionErr()}
	run := newTestJournal(t)
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}, Journal: run}

	_, err := ev.Evaluate(context.Background(), retryGate(nil),
		apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err == nil {
		t.Fatal("Evaluate returned nil, want fail-fast when no RetryPolicy is declared")
	}

	if rev.calls != 1 {
		t.Errorf("reviewer called %d times, want 1 (no declared retry = single attempt)", rev.calls)
	}
}

func TestEvaluateRetriesInvalidNeedsHumanVerdictWithCorrection(t *testing.T) {
	rev := &correctingReviewer{}
	ev := &Evaluator{
		Reviewer:           &ReviewerEvaluator{Goober: rev},
		IsNeedsHumanTarget: func(target string) bool { return target == "human" },
	}
	g := retryGate(&apiv1.RetryPolicy{MaxAttempts: 2})
	g.Branches[OutcomeFail] = "human"

	result, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if rev.calls != 2 {
		t.Fatalf("reviewer calls = %d, want 2", rev.calls)
	}
	if result.Target != "human" || result.Escalated {
		t.Fatalf("result = %+v, want the valid needs-human disposition", result)
	}
	if len(rev.addenda) != 2 || !strings.Contains(rev.addenda[1], "must end with") {
		t.Fatalf("retry addendum = %q, want actionable rationale correction", rev.addenda)
	}
}

func TestEvaluateExhaustedInvalidNeedsHumanVerdictEscalatesWithArtifact(t *testing.T) {
	rev := &correctingReviewer{invalid: true}
	run := newTestJournal(t)
	ev := &Evaluator{
		Reviewer:           &ReviewerEvaluator{Goober: rev},
		Journal:            run,
		IsNeedsHumanTarget: func(target string) bool { return target == "human" },
	}
	g := retryGate(&apiv1.RetryPolicy{MaxAttempts: 2})
	g.Branches[OutcomeFail] = "human"
	g.Branches[wf.BranchEscalate] = "remediation"

	result, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Target != "remediation" || !result.Escalated {
		t.Fatalf("result = %+v, want escalated remediation target", result)
	}
	if result.Verdict == nil || result.VerdictArtifact == nil {
		t.Fatalf("result = %+v, want invalid verdict and preserved artifact", result)
	}
	if result.Verdict.Rationale != "The approach needs a policy decision." {
		t.Fatalf("preserved rationale = %q, want original invalid rationale", result.Verdict.Rationale)
	}
}
