package gate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// fakeAutomated returns outcomes from a queue, one per call, so a test can
// script a gate through fail -> fail -> pass sequences.
type fakeAutomated struct {
	outcomes []string
	i        int
	err      error
	onCall   func()
}

type deadlineCapturingAutomated struct {
	deadline time.Time
	ok       bool
}

func (d *deadlineCapturingAutomated) Evaluate(ctx context.Context, _ apiv1.AutomatedGate, _ apiv1.InvocationEnvelope) (string, error) {
	d.deadline, d.ok = ctx.Deadline()
	return OutcomePass, nil
}

func (f *fakeAutomated) Evaluate(_ context.Context, _ apiv1.AutomatedGate, _ apiv1.InvocationEnvelope) (string, error) {
	if f.onCall != nil {
		f.onCall()
	}
	if f.err != nil {
		return "", f.err
	}
	if f.i >= len(f.outcomes) {
		return "", errors.New("fakeAutomated: no more scripted outcomes")
	}
	out := f.outcomes[f.i]
	f.i++
	return out, nil
}

// fixtureSpec builds the workflow the issue #20 acceptance criteria describes:
// implement -> automated gate -> (fail: repass to implement, pass: reviewgate)
// reviewgate -> (needs-changes: repass to implement, pass: complete, fail: @abort)
func fixtureSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "g",
		Start:    "implement",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@daily"}},
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goal: "implement the change", Goober: "coder", Next: "autogate"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "autogate",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{OutcomePass: "reviewgate", OutcomeFail: "implement"},
			},
			{
				Name:      "reviewgate",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					string(apiv1.VerdictPass):         wf.TerminalComplete,
					string(apiv1.VerdictNeedsChanges): "implement",
					string(apiv1.VerdictFail):         wf.TargetAbort,
				},
			},
		},
	}
}

func newTestJournal(t *testing.T) *journal.Run {
	t.Helper()
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "run-1", Workflow: "wf", Gaggle: "g"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return run
}

func readGateEvents(t *testing.T, run *journal.Run) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(run.Dir())
	if err != nil {
		t.Fatalf("journal.OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var gates []journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventGateEvaluated {
			gates = append(gates, ev)
		}
	}
	return gates
}

func TestEvaluatorJournalsStartBeforeDispatch(t *testing.T) {
	run := newTestJournal(t)
	auto := &fakeAutomated{outcomes: []string{OutcomePass}}
	auto.onCall = func() {
		rd, err := journal.OpenRead(run.Dir())
		if err != nil {
			t.Fatalf("OpenRead during evaluator dispatch: %v", err)
		}

		events, err := rd.Events()
		if err != nil {
			t.Fatalf("Events during evaluator dispatch: %v", err)
		}
		last := events[len(events)-1]
		if last.Type != journal.EventGateStarted || last.Gate != "autogate" {
			t.Fatalf("last event at evaluator dispatch = %+v, want gate.started for autogate", last)
		}
		if got := int(last.Runner["repassAttempt"].(float64)); got != 1 {
			t.Fatalf("gate.started repassAttempt = %d, want 1", got)
		}
	}

	ev := &Evaluator{Automated: auto, Journal: run}
	g := apiv1.Gate{
		Name:      "autogate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{OutcomePass: wf.TerminalComplete, OutcomeFail: "implement"},
	}

	if _, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
}

func TestEvaluatorAppliesInvocationTimeout(t *testing.T) {
	auto := &deadlineCapturingAutomated{}
	ev := &Evaluator{Automated: auto}
	g := apiv1.Gate{
		Name:      "autogate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals", TimeoutSeconds: 2},
		Branches:  map[string]string{OutcomePass: wf.TerminalComplete, OutcomeFail: wf.TargetAbort},
	}
	start := time.Now()
	env := apiv1.InvocationEnvelope{Limits: apiv1.Limits{MaxDurationSeconds: 2}}
	if _, err := ev.Evaluate(context.Background(), g, env, "implement", apiv1.ResultEnvelope{}, "", false); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !auto.ok {
		t.Fatal("automated evaluator context has no deadline")
	}
	if remaining := auto.deadline.Sub(start); remaining < time.Second || remaining > 3*time.Second {
		t.Fatalf("evaluator deadline remaining = %s, want approximately 2s", remaining)
	}
}

// TestFullRepassFixture drives the exact acceptance-criteria scenario:
// implement -> automated gate (fail) -> repass -> pass -> reviewer gate
// (needs-changes) -> repass -> approve -> complete, and asserts the journal
// shows every verdict and the loop count.
func TestFullRepassFixture(t *testing.T) {
	spec := fixtureSpec()
	if _, err := wf.Compile(wf.Definition{Name: "wf", Version: 1, Spec: spec}, wf.WithPreviewFeatures(true)); err != nil {
		t.Fatalf("Compile: %v", err)
	}

	auto := &fakeAutomated{outcomes: []string{OutcomeFail, OutcomePass}}
	rev := &fakeGoober{}
	run := newTestJournal(t)
	ev := &Evaluator{Automated: auto, Reviewer: &ReviewerEvaluator{Goober: rev}, Journal: run}

	autoGate := spec.Gates[0]
	reviewGate := spec.Gates[1]
	subject := apiv1.ResultEnvelope{Status: apiv1.ResultFailure}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: string(subject.Status)}}

	// 1st automated evaluation: fail -> repass to implement.
	r1, err := ev.Evaluate(context.Background(), autoGate, env, "implement", subject, "", false)
	if err != nil {
		t.Fatalf("Evaluate #1: %v", err)
	}
	if r1.Target != "implement" || r1.Attempt != 1 || r1.Escalated {
		t.Fatalf("r1 = %+v, want target=implement attempt=1 escalated=false", r1)
	}

	// "implement" repasses; this time it succeeds.
	env.Inputs[InputKeyStatus] = string(apiv1.ResultSuccess)
	r2, err := ev.Evaluate(context.Background(), autoGate, env, "implement", apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, "", false)
	if err != nil {
		t.Fatalf("Evaluate #2: %v", err)
	}
	if r2.Target != "reviewgate" || r2.Attempt != 0 || r2.Escalated {
		t.Fatalf("r2 = %+v, want target=reviewgate attempt=0 (reset) escalated=false", r2)
	}

	// Reviewer gate: needs-changes -> repass to implement.
	rev.reviewVerdict = apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "please fix X"}
	r3, err := ev.Evaluate(context.Background(), reviewGate, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate #3: %v", err)
	}
	if r3.Target != "implement" || r3.Attempt != 2 || r3.Escalated {
		t.Fatalf("r3 = %+v, want target=implement attempt=2 escalated=false", r3)
	}
	if r3.Verdict == nil || r3.Verdict.Summary != "please fix X" {
		t.Fatalf("r3.Verdict = %+v, want the reviewer's verdict attached", r3.Verdict)
	}

	// Reviewer gate: approve -> complete.
	rev.reviewVerdict = apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "looks good"}
	r4, err := ev.Evaluate(context.Background(), reviewGate, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate #4: %v", err)
	}
	if r4.Target != wf.TerminalComplete || r4.Attempt != 0 || r4.Escalated {
		t.Fatalf("r4 = %+v, want target=complete attempt=0 escalated=false", r4)
	}

	events := readGateEvents(t, run)
	if len(events) != 4 {
		t.Fatalf("journaled gate events = %d, want 4 (one per Evaluate call)", len(events))
	}
	wantGates := []string{"autogate", "autogate", "reviewgate", "reviewgate"}
	wantVerdicts := []string{OutcomeFail, OutcomePass, string(apiv1.VerdictNeedsChanges), string(apiv1.VerdictPass)}
	wantTargets := []string{"implement", "reviewgate", "implement", wf.TerminalComplete}
	wantAttempts := []int{1, 0, 2, 0}
	for i, ev := range events {
		if ev.Gate != wantGates[i] || ev.Verdict != wantVerdicts[i] || ev.Target != wantTargets[i] {
			t.Fatalf("event[%d] = %+v, want gate=%s verdict=%s target=%s", i, ev, wantGates[i], wantVerdicts[i], wantTargets[i])
		}
		gotAttempt, ok := ev.Runner["repassAttempt"]
		if !ok {
			t.Fatalf("event[%d] missing runner.repassAttempt: %+v", i, ev)
		}
		if int(gotAttempt.(float64)) != wantAttempts[i] {
			t.Fatalf("event[%d] repassAttempt = %v, want %d", i, gotAttempt, wantAttempts[i])
		}
	}
	// The reviewer gate's verdict (rationale/evidence/findings detail) must be
	// journaled as an artifact the events reference.
	if events[2].Ref == nil || events[2].Name == "" {
		t.Fatalf("needs-changes event missing verdict artifact ref: %+v", events[2])
	}
}

// TestEvaluateSetsVerdictArtifactForAgenticGate is issue #412's gate-side
// acceptance: Result.VerdictArtifact must point at the SAME artifact
// recordVerdict just journaled (not merely be present), so the runner's
// repass ContextPointer resolves to real reviewer content, not a
// placeholder — and must be absent wherever there is nothing to surface
// (automated gates, and journal disabled).
func TestEvaluateSetsVerdictArtifactForAgenticGate(t *testing.T) {
	spec := fixtureSpec()
	reviewGate := spec.Gates[1]
	rev := &fakeGoober{reviewVerdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "please fix X"}}
	run := newTestJournal(t)
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}, Journal: run}

	r, err := ev.Evaluate(context.Background(), reviewGate, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if r.VerdictArtifact == nil || r.VerdictArtifact.Digest == "" {
		t.Fatalf("VerdictArtifact = %+v, want a digested artifact pointer", r.VerdictArtifact)
	}

	events := readGateEvents(t, run)
	if len(events) != 1 || events[0].Ref == nil {
		t.Fatalf("journaled events = %+v, want exactly 1 with a Ref", events)
	}
	if r.VerdictArtifact.Digest != events[0].Ref.Digest {
		t.Fatalf("VerdictArtifact.Digest = %q, want the same digest journaled at events[0].Ref.Digest = %q", r.VerdictArtifact.Digest, events[0].Ref.Digest)
	}

	t.Run("nil for an automated gate (no Verdict to surface)", func(t *testing.T) {
		g := apiv1.Gate{
			Name: "autogate", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals"},
			Branches:  map[string]string{OutcomePass: "", OutcomeFail: "implement"},
		}
		auto := &fakeAutomated{outcomes: []string{OutcomeFail}}
		ev := &Evaluator{Automated: auto, Journal: newTestJournal(t)}
		r, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "failure"}}, "implement", apiv1.ResultEnvelope{}, "", false)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if r.VerdictArtifact != nil {
			t.Fatalf("VerdictArtifact = %+v, want nil (automated gates have no Verdict)", r.VerdictArtifact)
		}
	})

	t.Run("nil when journaling is disabled", func(t *testing.T) {
		rev := &fakeGoober{reviewVerdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges}}
		ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}}
		r, err := ev.Evaluate(context.Background(), reviewGate, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if r.VerdictArtifact != nil {
			t.Fatalf("VerdictArtifact = %+v, want nil (no Journal to record into)", r.VerdictArtifact)
		}
	})
}

// TestEvaluatorEscalatesOnRepassBudgetExhaustion proves loop budget exhaustion
// routes to @escalate instead of infinitely looping.
func TestEvaluatorEscalatesOnRepassBudgetExhaustion(t *testing.T) {
	g := apiv1.Gate{
		Name:      "autogate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{OutcomePass: "", OutcomeFail: "implement"},
	}

	auto := &fakeAutomated{outcomes: []string{OutcomeFail, OutcomeFail, OutcomeFail}}
	run := newTestJournal(t)
	ev := &Evaluator{Automated: auto, MaxRepasses: 2, Journal: run}

	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "failure"}}
	subject := apiv1.ResultEnvelope{Status: apiv1.ResultFailure}

	for i, want := range []struct {
		target    string
		escalated bool
	}{
		{"implement", false},      // attempt 1 <= budget(2)
		{"implement", false},      // attempt 2 <= budget(2)
		{wf.TargetEscalate, true}, // attempt 3 > budget(2)
	} {
		r, err := ev.Evaluate(context.Background(), g, env, "implement", subject, "", false)
		if err != nil {
			t.Fatalf("Evaluate #%d: %v", i, err)
		}
		if r.Target != want.target || r.Escalated != want.escalated {
			t.Fatalf("Evaluate #%d = %+v, want target=%s escalated=%v", i, r, want.target, want.escalated)
		}
	}

	events := readGateEvents(t, run)
	if len(events) != 3 {
		t.Fatalf("journaled gate events = %d, want 3", len(events))
	}
	if events[2].Target != wf.TargetEscalate {
		t.Fatalf("last journaled target = %q, want %q", events[2].Target, wf.TargetEscalate)
	}
}

func TestEvaluatorUsesPerGateRepassBudget(t *testing.T) {
	g := apiv1.Gate{
		Name:        "autogate",
		Evaluator:   apiv1.EvaluatorAutomated,
		Automated:   &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:    map[string]string{OutcomePass: "", OutcomeFail: "implement"},
		MaxRepasses: 1,
	}
	ev := &Evaluator{
		Automated:   &fakeAutomated{outcomes: []string{OutcomeFail, OutcomeFail}},
		MaxRepasses: 5,
	}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "failure"}}
	if first, err := ev.Evaluate(context.Background(), g, env, "implement", apiv1.ResultEnvelope{}, "", false); err != nil || first.Escalated {
		t.Fatalf("first evaluation = %+v, %v; want ordinary repass", first, err)
	}
	if second, err := ev.Evaluate(context.Background(), g, env, "implement", apiv1.ResultEnvelope{}, "", false); err != nil || !second.Escalated {
		t.Fatalf("second evaluation = %+v, %v; want gate-budget escalation", second, err)
	}
}

func TestEvaluatorSeparatesInfrastructureAndPolicyRepassBudgets(t *testing.T) {
	g := apiv1.Gate{
		Name:      "local-gate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "failure-class"},
		Branches: map[string]string{
			OutcomePass:       "open-pr",
			OutcomeFail:       "implement",
			OutcomeInfra:      "local-ci",
			wf.BranchEscalate: "park-escalated",
		},
	}
	ev := &Evaluator{
		Automated:   &fakeAutomated{outcomes: []string{OutcomeInfra, OutcomeInfra, OutcomeInfra, OutcomeFail, OutcomeInfra}},
		MaxRepasses: 1,
		IsReentry:   func(target string) bool { return target == "local-ci" || target == "implement" },
	}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "failure"}}
	subject := apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: "timeout", Retryable: true},
	}

	for attempt := 1; attempt <= DefaultMaxInfrastructureRepasses; attempt++ {
		result, err := ev.Evaluate(context.Background(), g, env, "local-ci", subject, "", false)
		if err != nil {
			t.Fatalf("infrastructure evaluation %d: %v", attempt, err)
		}
		if result.Target != "local-ci" || result.Attempt != attempt || result.Escalated {
			t.Fatalf("infrastructure evaluation %d = %+v, want bounded retry to local-ci", attempt, result)
		}
	}
	exhausted, err := ev.Evaluate(context.Background(), g, env, "local-ci", subject, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !exhausted.Escalated || exhausted.Target != "park-escalated" ||
		exhausted.Attempt != DefaultMaxInfrastructureRepasses+1 {
		t.Fatalf("exhausted infrastructure evaluation = %+v", exhausted)
	}
	if got := ev.RepassAttempts["local-ci"]; got != 0 {
		t.Fatalf("policy repasses for local-ci = %d, want 0", got)
	}

	policy, err := ev.Evaluate(context.Background(), g, env, "local-ci", subject, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Escalated || policy.Target != "implement" || policy.Attempt != 1 {
		t.Fatalf("first policy repass after infrastructure retries = %+v", policy)
	}
	restarted, err := ev.Evaluate(context.Background(), g, env, "local-ci", subject, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Escalated || restarted.Target != "local-ci" || restarted.Attempt != 1 {
		t.Fatalf("infrastructure budget after policy remediation = %+v, want fresh retry budget", restarted)
	}
}

func TestEvaluatorRoutesRepassEscalationThroughControlBranch(t *testing.T) {
	g := apiv1.Gate{
		Name:      "review",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches: map[string]string{
			OutcomePass:       "",
			OutcomeFail:       "implement",
			wf.BranchEscalate: "park-needs-human",
		},
	}
	auto := &fakeAutomated{outcomes: []string{OutcomeFail, OutcomeFail}}
	run := newTestJournal(t)
	ev := &Evaluator{Automated: auto, MaxRepasses: 1, Journal: run}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "failure"}}

	first, err := ev.Evaluate(context.Background(), g, env, "implement", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate #1: %v", err)
	}
	if first.Target != "implement" || first.Escalated {
		t.Fatalf("Evaluate #1 = %+v, want ordinary repass to implement", first)
	}

	second, err := ev.Evaluate(context.Background(), g, env, "implement", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate #2: %v", err)
	}
	if second.Target != "park-needs-human" || !second.Escalated {
		t.Fatalf("Evaluate #2 = %+v, want escalated target park-needs-human", second)
	}

	events := readGateEvents(t, run)
	if got := events[len(events)-1].Target; got != "park-needs-human" {
		t.Fatalf("last journaled target = %q, want park-needs-human", got)
	}
	if !events[len(events)-1].Escalated {
		t.Fatal("last journaled gate event did not record escalated=true")
	}

	recovered, ok, err := (&Evaluator{
		MaxRepasses: 1,
		Attempts:    map[string]int{"review": 2},
		Journal:     newTestJournal(t),
	}).RecoverInterrupted(g, "")
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if !ok || recovered.Target != "park-needs-human" || !recovered.Escalated {
		t.Fatalf("RecoverInterrupted = %+v,%v, want escalated target park-needs-human", recovered, ok)
	}
}

// TestEvaluatorEscalatesOnDuplicateDiffWithoutReReview is issue #316's core
// acceptance: an implementer stuck in a non-convergent repass loop produces a
// diff byte-identical to its immediately prior attempt. The second such
// attempt must escalate on the spot — not after burning the rest of the
// repass budget — and, critically, must never re-invoke the (real, costly)
// reviewer for a diff it has already judged.
func TestEvaluatorEscalatesOnDuplicateDiffWithoutReReview(t *testing.T) {
	g := apiv1.Gate{
		Name:      "reviewgate",
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
		Branches: map[string]string{
			string(apiv1.VerdictPass):         wf.TerminalComplete,
			string(apiv1.VerdictNeedsChanges): "implement",
			string(apiv1.VerdictFail):         wf.TargetAbort,
		},
	}
	rev := &fakeGoober{reviewVerdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "please fix X"}}
	run := newTestJournal(t)
	// MaxRepasses is generous (3) so escalation on the very next attempt can
	// only be explained by the duplicate-diff detection, not budget exhaustion.
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}, MaxRepasses: 3, Journal: run}

	// 1st attempt: a real diff, never seen before — reviewer is consulted.
	r1, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "sha256:aaaa", false)
	if err != nil {
		t.Fatalf("Evaluate #1: %v", err)
	}
	if r1.Target != "implement" || r1.Attempt != 1 || r1.Escalated || r1.DuplicateDiff {
		t.Fatalf("r1 = %+v, want target=implement attempt=1 escalated=false duplicateDiff=false", r1)
	}
	if rev.reviewCalls != 1 {
		t.Fatalf("reviewCalls after #1 = %d, want 1", rev.reviewCalls)
	}

	// 2nd attempt: the implementer produced the exact same diff again (the
	// #316 failure mode) — Evaluate must detect the repeat, escalate, and
	// skip the reviewer call entirely.
	r2, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "sha256:aaaa", false)
	if err != nil {
		t.Fatalf("Evaluate #2: %v", err)
	}
	if r2.Target != wf.TargetEscalate || !r2.Escalated || !r2.DuplicateDiff {
		t.Fatalf("r2 = %+v, want target=%s escalated=true duplicateDiff=true", r2, wf.TargetEscalate)
	}
	if r2.Attempt != 2 {
		t.Fatalf("r2.Attempt = %d, want 2 (well under the budget of 3 — escalation is from the duplicate, not the budget)", r2.Attempt)
	}
	if rev.reviewCalls != 1 {
		t.Fatalf("reviewCalls after #2 = %d, want still 1 (the reviewer must not be re-invoked on a detected duplicate)", rev.reviewCalls)
	}
	if r2.Verdict == nil || r2.Verdict.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("r2.Verdict = %+v, want a synthesized needs-changes verdict explaining the escalation", r2.Verdict)
	}

	events := readGateEvents(t, run)
	if len(events) != 2 {
		t.Fatalf("journaled gate events = %d, want 2", len(events))
	}
	dup, ok := events[1].Runner["duplicateDiff"].(bool)
	if !ok || !dup {
		t.Fatalf("events[1].Runner[duplicateDiff] = %v, want true", events[1].Runner["duplicateDiff"])
	}
	if digest, _ := events[1].Runner["diffDigest"].(string); digest != "sha256:aaaa" {
		t.Fatalf("events[1].Runner[diffDigest] = %q, want sha256:aaaa", digest)
	}
}

func TestEvaluatorDuplicateDiffNamesUpstreamRepassCause(t *testing.T) {
	g := apiv1.Gate{
		Name: "review", Evaluator: apiv1.EvaluatorAgentic,
		Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
		Branches: map[string]string{
			string(apiv1.VerdictPass): wf.TerminalComplete, string(apiv1.VerdictNeedsChanges): "implement",
		},
	}
	tests := []struct {
		name  string
		cause RepassCause
		want  []string
	}{
		{
			name: "CI failure",
			cause: RepassCause{
				Kind: "stage-failure", Gate: "local-gate", Outcome: "fail", Stage: "local-ci",
				ErrorCode: "deadline_exceeded", ErrorMessage: "exceeded its 10m timeout",
			},
			want: []string{"local-gate", "local-ci", "deadline_exceeded", "10m timeout", "produced no change"},
		},
		{
			name:  "reviewer needs changes",
			cause: RepassCause{Kind: "reviewer", Gate: "review", Outcome: "needs-changes", Rationale: "the parser still accepts empty input"},
			want:  []string{"review", "needs-changes", "parser still accepts empty input", "produced no change"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := newTestJournal(t)
			ev := &Evaluator{
				Reviewer:       &ReviewerEvaluator{Goober: &fakeGoober{}},
				Journal:        run,
				LastDiffDigest: map[string]string{"review": "sha256:same"},
				RepassCause:    &tc.cause,
			}
			result, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "sha256:same", false)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if result.RepassCause == nil || result.Verdict == nil {
				t.Fatalf("result = %+v, want machine-readable cause and synthesized verdict", result)
			}
			for _, want := range tc.want {
				if !strings.Contains(result.Verdict.Rationale, want) {
					t.Fatalf("rationale = %q, want %q", result.Verdict.Rationale, want)
				}
			}
			events := readGateEvents(t, run)
			cause, ok := events[0].Runner["repassCause"].(map[string]any)
			if !ok || cause["kind"] != tc.cause.Kind || cause["gate"] != tc.cause.Gate {
				t.Fatalf("journal repassCause = %#v, want kind=%q gate=%q", events[0].Runner["repassCause"], tc.cause.Kind, tc.cause.Gate)
			}
		})
	}
}

// TestEvaluatorUnchangedRepassClassifiesInfrastructureVsImplementation is
// issue #3250's core acceptance: a duplicate-diff escalation must carry a
// stable, matchable Reason (ReasonUnchangedRepass) and must classify whether
// the upstream failure that triggered the unproductive repass was itself an
// infrastructure/environment failure (RepassCause.Infrastructure, set by the
// runner from the failed attempt's own recorded AttemptClass) — distinct
// from an ordinary implementation defect the implementer failed to converge
// on — so downstream consumers (escalation notifications, `goobers
// trace`/status) never conflate the two.
func TestEvaluatorUnchangedRepassClassifiesInfrastructureVsImplementation(t *testing.T) {
	g := apiv1.Gate{
		Name: "review", Evaluator: apiv1.EvaluatorAgentic,
		Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
		Branches: map[string]string{
			string(apiv1.VerdictPass): wf.TerminalComplete, string(apiv1.VerdictNeedsChanges): "implement",
		},
	}
	tests := []struct {
		name           string
		cause          RepassCause
		wantInfraWords []string
	}{
		{
			name: "environmental CI failure",
			cause: RepassCause{
				Kind: "stage-failure", Gate: "local-gate", Outcome: "fail", Stage: "local-ci",
				ErrorCode: "host_contention", ErrorMessage: "runner host under contention",
				Infrastructure: true,
			},
			wantInfraWords: []string{"infrastructure/environment"},
		},
		{
			name: "genuine implementation failure",
			cause: RepassCause{
				Kind: "stage-failure", Gate: "local-gate", Outcome: "fail", Stage: "local-ci",
				ErrorCode: "test_failure", ErrorMessage: "TestFoo failed",
				Infrastructure: false,
			},
			wantInfraWords: []string{"an implementation"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := newTestJournal(t)
			ev := &Evaluator{
				Reviewer:       &ReviewerEvaluator{Goober: &fakeGoober{}},
				Journal:        run,
				LastDiffDigest: map[string]string{"review": "sha256:same"},
				RepassCause:    &tc.cause,
			}
			result, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "sha256:same", false)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if !result.DuplicateDiff || result.Reason != ReasonUnchangedRepass {
				t.Fatalf("result = %+v, want DuplicateDiff=true Reason=%q", result, ReasonUnchangedRepass)
			}
			for _, want := range tc.wantInfraWords {
				if !strings.Contains(result.Verdict.Rationale, want) {
					t.Fatalf("rationale = %q, want substring %q", result.Verdict.Rationale, want)
				}
			}
			events := readGateEvents(t, run)
			if reason, _ := events[0].Runner["reason"].(string); reason != ReasonUnchangedRepass {
				t.Fatalf("journaled reason = %q, want %q", reason, ReasonUnchangedRepass)
			}
			cause, ok := events[0].Runner["repassCause"].(map[string]any)
			if !ok {
				t.Fatalf("journal repassCause missing: %#v", events[0].Runner["repassCause"])
			}
			if infra, _ := cause["infrastructure"].(bool); infra != tc.cause.Infrastructure {
				t.Fatalf("journaled repassCause.infrastructure = %v, want %v", infra, tc.cause.Infrastructure)
			}
		})
	}
}

// TestEvaluatorReusesCachedVerdictWithoutReviewerCall is issue #523's core
// mechanism test: when the caller (merge-review's gather-sibling-context, in
// production; the test itself here) has already found a digest-matched
// prior verdict and sets Evaluator.CachedVerdict, Evaluate must skip the
// reviewer entirely, reuse that verdict's content verbatim (Decision,
// Digest, SourceRunID all unchanged), resolve the branch from ITS Decision,
// and journal CacheHit=true — auditable via the same Runner-namespace
// annotation convention duplicateDiff already established.
func TestEvaluatorReusesCachedVerdictWithoutReviewerCall(t *testing.T) {
	g := apiv1.Gate{
		Name:      "reviewgate",
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
		Branches: map[string]string{
			string(apiv1.VerdictPass):         wf.TerminalComplete,
			string(apiv1.VerdictNeedsChanges): "implement",
			string(apiv1.VerdictFail):         wf.TargetAbort,
		},
	}
	rev := &fakeGoober{reviewVerdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "a live call would say this"}}
	run := newTestJournal(t)
	cached := &apiv1.Verdict{
		Decision: apiv1.VerdictPass, Summary: "reused from a prior run",
		Digest: "sha256:matched", SourceRunID: "run-original", HeadSHA: "headsha1", BaseSHA: "basesha1",
	}
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}, Journal: run, CachedVerdict: cached}

	r, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "gather-sibling-context", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if rev.reviewCalls != 0 {
		t.Fatalf("reviewCalls = %d, want 0 (the reviewer must never be invoked on a cache hit)", rev.reviewCalls)
	}
	if !r.CacheHit {
		t.Fatalf("r.CacheHit = false, want true")
	}
	if r.Target != wf.TerminalComplete {
		t.Fatalf("r.Target = %q, want %q (resolved from the cached verdict's own pass decision)", r.Target, wf.TerminalComplete)
	}
	if r.Verdict == nil || r.Verdict.Digest != "sha256:matched" || r.Verdict.SourceRunID != "run-original" {
		t.Fatalf("r.Verdict = %+v, want the cached verdict reused verbatim (Digest/SourceRunID unchanged)", r.Verdict)
	}
	if r.DuplicateDiff {
		t.Fatalf("r.DuplicateDiff = true, want false (CacheHit and DuplicateDiff are distinct escalation-free vs. escalating paths)")
	}

	events := readGateEvents(t, run)
	if len(events) != 1 {
		t.Fatalf("journaled gate events = %d, want 1", len(events))
	}
	hit, ok := events[0].Runner["verdictCacheHit"].(bool)
	if !ok || !hit {
		t.Fatalf("events[0].Runner[verdictCacheHit] = %v, want true", events[0].Runner["verdictCacheHit"])
	}

	t.Run("rebinding CachedVerdict to nil for the next gate restores live evaluation", func(t *testing.T) {
		ev.CachedVerdict = nil
		r2, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "gather-sibling-context", apiv1.ResultEnvelope{}, "", false)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if r2.CacheHit {
			t.Fatalf("r2.CacheHit = true, want false once CachedVerdict is rebound to nil")
		}
		if rev.reviewCalls != 1 {
			t.Fatalf("reviewCalls = %d, want 1 (the live reviewer must run once CachedVerdict is cleared)", rev.reviewCalls)
		}
	})
}

func TestEvaluatorEscalatesCachedInvalidNeedsHumanVerdict(t *testing.T) {
	g := apiv1.Gate{
		Name:      "reviewgate",
		Evaluator: apiv1.EvaluatorAgentic,
		Branches: map[string]string{
			string(apiv1.VerdictPass): wf.TerminalComplete,
			string(apiv1.VerdictFail): "human",
			wf.BranchEscalate:         "remediation",
		},
	}
	run := newTestJournal(t)
	cached := &apiv1.Verdict{
		Decision:    apiv1.VerdictFail,
		Rationale:   "The approach needs a policy decision.",
		SourceRunID: "run-original",
	}
	ev := &Evaluator{
		Journal:       run,
		CachedVerdict: cached,
		IsNeedsHumanTarget: func(target string) bool {
			return target == "human"
		},
	}

	result, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "review", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Target != "remediation" || !result.Escalated || !result.CacheHit {
		t.Fatalf("result = %+v, want escalated cached verdict to remediation", result)
	}
	if result.Verdict != cached || result.VerdictArtifact == nil {
		t.Fatalf("result = %+v, want cached verdict and preserved artifact", result)
	}
	events := readGateEvents(t, run)
	if len(events) != 1 || events[0].Target != "remediation" || !events[0].Escalated {
		t.Fatalf("gate events = %+v, want one escalated remediation event", events)
	}
}

// TestEvaluatorFastFailsEmptyDiffOnReviewOne is issue #415's reviewer sibling:
// when the implement stage commits nothing (an empty diff — e.g. it produced
// no change on an over-scope probe), the reviewer gate must synthesize `fail`
// evidence on the FIRST review while routing through the mechanical escalation
// branch, and must never invoke the (real, costly) reviewer for a diff that
// offers nothing to evaluate. Without it, an empty diff can be mistaken for a
// genuine reviewer rejection requiring a human decision.
func TestEvaluatorFastFailsEmptyDiffOnReviewOne(t *testing.T) {
	g := apiv1.Gate{
		Name:      "reviewgate",
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
		Branches: map[string]string{
			string(apiv1.VerdictPass):         wf.TerminalComplete,
			string(apiv1.VerdictNeedsChanges): "implement",
			string(apiv1.VerdictFail):         "park-needs-human",
			wf.BranchEscalate:                 "park-escalated",
		},
	}
	// The fake would PASS if consulted — so a `fail` outcome can only come from
	// the empty-diff short-circuit, never from the reviewer.
	rev := &fakeGoober{reviewVerdict: apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "would approve"}}
	run := newTestJournal(t)
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}, MaxRepasses: 3, Journal: run}

	// emptyDiff=true, diffDigest="" — the run branch carries no committed change.
	r, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", true)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if r.Target != "park-escalated" || r.Outcome != string(apiv1.VerdictFail) {
		t.Fatalf("r = %+v, want outcome=fail target=park-escalated (the mechanical escalation branch)", r)
	}
	if !r.Escalated || r.DuplicateDiff {
		t.Fatalf("r = %+v, want escalated=true duplicateDiff=false — an empty diff is a distinct first-review execution stall", r)
	}
	if r.Attempt != 1 {
		t.Fatalf("r.Attempt = %d, want 1 (fails on the first review, no repass loop)", r.Attempt)
	}
	if rev.reviewCalls != 0 {
		t.Fatalf("reviewCalls = %d, want 0 (the reviewer must never be invoked for an empty diff)", rev.reviewCalls)
	}
	if r.Verdict == nil || r.Verdict.Decision != apiv1.VerdictFail {
		t.Fatalf("r.Verdict = %+v, want a synthesized fail verdict explaining the empty diff", r.Verdict)
	}
}

// TestEvaluatorDoesNotEscalateOnDifferentDiff is the control for the
// duplicate-diff test above: consecutive attempts with genuinely different
// diffs are ordinary repass activity, not non-convergence, and must not
// trip the #316 short-circuit — proving escalation above was caused by the
// matching digest, not merely by two consecutive non-pass attempts.
func TestEvaluatorDoesNotEscalateOnDifferentDiff(t *testing.T) {
	g := apiv1.Gate{
		Name:      "reviewgate",
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
		Branches: map[string]string{
			string(apiv1.VerdictPass):         wf.TerminalComplete,
			string(apiv1.VerdictNeedsChanges): "implement",
			string(apiv1.VerdictFail):         wf.TargetAbort,
		},
	}
	rev := &fakeGoober{reviewVerdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges}}
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: rev}, MaxRepasses: 3, Journal: newTestJournal(t)}

	if _, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "sha256:aaaa", false); err != nil {
		t.Fatalf("Evaluate #1: %v", err)
	}
	r2, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "sha256:bbbb", false)
	if err != nil {
		t.Fatalf("Evaluate #2: %v", err)
	}
	if r2.Escalated || r2.DuplicateDiff || r2.Target != "implement" {
		t.Fatalf("r2 = %+v, want target=implement escalated=false duplicateDiff=false (a genuinely new diff each time)", r2)
	}
	if rev.reviewCalls != 2 {
		t.Fatalf("reviewCalls = %d, want 2 (reviewer consulted for every genuinely new diff)", rev.reviewCalls)
	}
}

func TestEvaluatorBoundsRepassTargetAcrossDistinctGates(t *testing.T) {
	review := apiv1.Gate{
		Name: "review", Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{OutcomePass: "local-ci", OutcomeFail: "implement"},
	}
	localGate := apiv1.Gate{
		Name: "local-gate", Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{OutcomePass: wf.TerminalComplete, OutcomeFail: "implement"},
	}
	ev := &Evaluator{MaxRepasses: 2, Journal: newTestJournal(t)}

	steps := []struct {
		gate    apiv1.Gate
		outcome string
		want    int
	}{
		{review, OutcomePass, 0},
		{localGate, OutcomeFail, 1},
		{review, OutcomeFail, 2},
		{review, OutcomePass, 0},
		{localGate, OutcomeFail, 3},
	}
	var result Result
	for _, step := range steps {
		var err error
		result, err = ev.EvaluateKnownOutcome(step.gate, step.outcome)
		if err != nil {
			t.Fatalf("EvaluateKnownOutcome(%s, %s): %v", step.gate.Name, step.outcome, err)
		}
		if result.Attempt != step.want {
			t.Fatalf("EvaluateKnownOutcome(%s, %s) attempt = %d, want %d", step.gate.Name, step.outcome, result.Attempt, step.want)
		}
	}
	if !result.Escalated || result.Target != wf.TargetEscalate {
		t.Fatalf("final result = %+v, want repass-budget escalation", result)
	}
	if result.DuplicateDiff {
		t.Fatal("final result used duplicate-diff guard, want repass-budget escalation")
	}
	if got := ev.RepassAttempts["implement"]; got != 3 {
		t.Fatalf("RepassAttempts[implement] = %d, want 3", got)
	}
}

func TestEvaluatorChargesPassBranchWhenItReentersStage(t *testing.T) {
	g := apiv1.Gate{
		Name: "review", Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{OutcomePass: "implement", OutcomeFail: wf.TargetAbort},
	}
	ev := &Evaluator{
		MaxRepasses:    1,
		Attempts:       map[string]int{"review": 1},
		RepassAttempts: map[string]int{"implement": 1},
		IsReentry:      func(target string) bool { return target == "implement" },
	}

	result, err := ev.EvaluateKnownOutcome(g, OutcomePass)
	if err != nil {
		t.Fatalf("EvaluateKnownOutcome: %v", err)
	}
	if !result.Escalated || result.Attempt != 2 || result.GateAttempt != 0 ||
		result.RepassTarget != "implement" || result.Target != wf.TargetEscalate {
		t.Fatalf("result = %+v, want pass-driven target-stage escalation with reset gate attempt", result)
	}
}

// TestEvaluatorHonorsSeededRepassCount is #89's gate-side acceptance test: a
// caller resuming an interrupted run (internal/runner.Resume) constructs a
// fresh Evaluator with Attempts pre-seeded from the run's last-known
// gate.evaluated event, rather than the zero value trackRepass would
// otherwise start from — so a crash mid-repass-loop can't grant a gate a
// fresh budget it hadn't earned pre-crash.
func TestEvaluatorHonorsSeededRepassCount(t *testing.T) {
	g := apiv1.Gate{
		Name:      "autogate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{OutcomePass: "", OutcomeFail: "implement"},
	}
	auto := &fakeAutomated{outcomes: []string{OutcomeFail}}
	run := newTestJournal(t)
	// Simulates a resumed Evaluator: pre-crash, "autogate" had already failed
	// twice (budget 2) — seeded exactly as internal/runner.Resume would,
	// reconstructing from the run's last gate.evaluated event rather than
	// starting fresh.
	ev := &Evaluator{
		Automated: auto, MaxRepasses: 2, Journal: run,
		Attempts: map[string]int{"autogate": 2}, RepassAttempts: map[string]int{"implement": 2},
	}

	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "failure"}}
	subject := apiv1.ResultEnvelope{Status: apiv1.ResultFailure}

	r, err := ev.Evaluate(context.Background(), g, env, "implement", subject, "", false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// The seeded count (2) plus this evaluation's own failure (1) is 3, which
	// exceeds the budget of 2 on this very first post-resume call — proving
	// the budget picked up where the pre-crash run left off instead of
	// resetting to a fresh 0/2.
	if r.Attempt != 3 || !r.Escalated || r.Target != wf.TargetEscalate {
		t.Fatalf("Evaluate = %+v, want attempt=3 escalated=true target=%s (seeded count honored)", r, wf.TargetEscalate)
	}
	if got := ev.Attempts["autogate"]; got != 3 {
		t.Fatalf("ev.Attempts[autogate] = %d, want 3 (live, inspectable for the next checkpoint)", got)
	}
	if got := ev.RepassAttempts["implement"]; got != 3 {
		t.Fatalf("ev.RepassAttempts[implement] = %d, want 3", got)
	}

	// A fresh, unseeded Evaluator against the identical sequence must NOT
	// escalate on the first call — the control proving the seed above, not
	// MaxRepasses or the fixture, is what drove the escalation.
	freshAuto := &fakeAutomated{outcomes: []string{OutcomeFail}}
	fresh := &Evaluator{Automated: freshAuto, MaxRepasses: 2, Journal: newTestJournal(t)}
	fr, err := fresh.Evaluate(context.Background(), g, env, "implement", subject, "", false)
	if err != nil {
		t.Fatalf("Evaluate (fresh): %v", err)
	}
	if fr.Attempt != 1 || fr.Escalated {
		t.Fatalf("Evaluate (fresh) = %+v, want attempt=1 escalated=false", fr)
	}
}

func TestEvaluatorEscalatesRecoveredInterruptedBudgetWithoutDispatch(t *testing.T) {
	g := apiv1.Gate{
		Name:      "autogate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{OutcomePass: wf.TerminalComplete, OutcomeFail: "implement"},
	}
	auto := &fakeAutomated{outcomes: []string{OutcomePass}}
	run := newTestJournal(t)
	ev := &Evaluator{
		Automated:   auto,
		Journal:     run,
		MaxRepasses: 1,
		Attempts:    map[string]int{"autogate": 2},
	}

	r, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if auto.i != 0 {
		t.Fatalf("automated evaluator calls = %d, want 0 after interrupted attempts exhausted the budget", auto.i)
	}
	if !r.Interrupted || !r.Escalated || r.Attempt != 2 || r.Target != wf.TargetEscalate {
		t.Fatalf("Evaluate = %+v, want interrupted escalation at recovered attempt 2", r)
	}
	events := readGateEvents(t, run)
	if len(events) != 1 {
		t.Fatalf("gate.evaluated events = %d, want 1 synthesized recovery verdict", len(events))
	}
	if interrupted, _ := events[0].Runner["interrupted"].(bool); !interrupted {
		t.Fatalf("gate.evaluated Runner[interrupted] = %v, want true", events[0].Runner["interrupted"])
	}
}

func TestEvaluatorNeverSilentlyPasses(t *testing.T) {
	g := apiv1.Gate{
		Name:      "autogate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{OutcomePass: "done"}, // no "fail" branch declared
	}
	auto := &fakeAutomated{outcomes: []string{OutcomeFail}}
	ev := &Evaluator{Automated: auto}
	if _, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "failure"}}, "implement", apiv1.ResultEnvelope{}, "", false); err == nil {
		t.Fatal("want error when the outcome has no defined branch (GT-002)")
	}
}

func TestEvaluatorHumanGateRequiresExplicitDecision(t *testing.T) {
	g := apiv1.Gate{
		Name:      "approve",
		Evaluator: apiv1.EvaluatorHuman,
		Human:     &apiv1.HumanGate{},
		Branches:  map[string]string{"pass": "done", "needs-changes": "implement", "reject": wf.TargetAbort},
	}
	run := newTestJournal(t)
	ev := &Evaluator{Journal: run}

	if _, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false); err == nil {
		t.Fatal("Evaluate without a human decision succeeded")
	}
	if _, err := ev.EvaluateHuman(g, "unknown", ""); err == nil {
		t.Fatal("EvaluateHuman accepted a decision with no branch")
	}
	restricted := g
	restricted.Human = &apiv1.HumanGate{Approvers: []string{"maintainers"}}
	if _, err := (&Evaluator{}).EvaluateHuman(restricted, "pass", ""); err == nil ||
		!strings.Contains(err.Error(), "actor is required") {
		t.Fatalf("EvaluateHuman without actor error = %v, want required actor", err)
	}
	if _, err := (&Evaluator{}).EvaluateHuman(restricted, "pass", "contributor"); err == nil ||
		!strings.Contains(err.Error(), "not an authorized approver") {
		t.Fatalf("EvaluateHuman unauthorized actor error = %v, want authorization failure", err)
	}
	if _, err := (&Evaluator{}).EvaluateHuman(restricted, "pass", "maintainers"); err != nil {
		t.Fatalf("EvaluateHuman authorized actor: %v", err)
	}

	got, err := ev.EvaluateHuman(g, "needs-changes", "")
	if err != nil {
		t.Fatalf("EvaluateHuman: %v", err)
	}
	if got.Outcome != "needs-changes" || got.Target != "implement" || got.Attempt != 0 || got.Escalated {
		t.Fatalf("EvaluateHuman = %+v, want needs-changes branch to implement", got)
	}
	events := readGateEvents(t, run)
	if len(events) != 1 || events[0].Type != journal.EventGateEvaluated ||
		events[0].Verdict != "needs-changes" || events[0].Target != "implement" {
		t.Fatalf("human gate events = %+v, want one gate.evaluated verdict", events)
	}
}

func TestEvaluatorRequiresConfiguredDependency(t *testing.T) {
	autoGate := apiv1.Gate{Name: "g", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "status-equals"}, Branches: map[string]string{OutcomePass: ""}}
	if _, err := (&Evaluator{}).Evaluate(context.Background(), autoGate, apiv1.InvocationEnvelope{}, "s", apiv1.ResultEnvelope{}, "", false); err == nil {
		t.Fatal("want error when Automated is not configured")
	}

	agenticGate := apiv1.Gate{Name: "g", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "r"}, Branches: map[string]string{"pass": ""}}
	if _, err := (&Evaluator{}).Evaluate(context.Background(), agenticGate, apiv1.InvocationEnvelope{}, "s", apiv1.ResultEnvelope{}, "", false); err == nil {
		t.Fatal("want error when Reviewer is not configured")
	}
}

func TestRecordVerdictNilJournalIsNoop(t *testing.T) {
	artifact, err := recordVerdict(nil, Result{Gate: "g", Outcome: "pass", Target: ""}, "")
	if err != nil {
		t.Fatalf("recordVerdict with nil Journal: %v", err)
	}
	if artifact != nil {
		t.Fatalf("recordVerdict with nil Journal returned artifact %+v, want nil", artifact)
	}
}
