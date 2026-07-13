package gate

import (
	"context"
	"errors"
	"testing"

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
}

func (f *fakeAutomated) Evaluate(_ context.Context, _ apiv1.AutomatedGate, _ apiv1.InvocationEnvelope) (string, error) {
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

// TestFullRepassFixture drives the exact acceptance-criteria scenario:
// implement -> automated gate (fail) -> repass -> pass -> reviewer gate
// (needs-changes) -> repass -> approve -> complete, and asserts the journal
// shows every verdict and the loop count.
func TestFullRepassFixture(t *testing.T) {
	spec := fixtureSpec()
	if _, err := wf.Compile(wf.Definition{Name: "wf", Version: 1, Spec: spec}); err != nil {
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
	r1, err := ev.Evaluate(context.Background(), autoGate, env, "implement", subject)
	if err != nil {
		t.Fatalf("Evaluate #1: %v", err)
	}
	if r1.Target != "implement" || r1.Attempt != 1 || r1.Escalated {
		t.Fatalf("r1 = %+v, want target=implement attempt=1 escalated=false", r1)
	}

	// "implement" repasses; this time it succeeds.
	env.Inputs[InputKeyStatus] = string(apiv1.ResultSuccess)
	r2, err := ev.Evaluate(context.Background(), autoGate, env, "implement", apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	if err != nil {
		t.Fatalf("Evaluate #2: %v", err)
	}
	if r2.Target != "reviewgate" || r2.Attempt != 0 || r2.Escalated {
		t.Fatalf("r2 = %+v, want target=reviewgate attempt=0 (reset) escalated=false", r2)
	}

	// Reviewer gate: needs-changes -> repass to implement.
	rev.reviewVerdict = apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "please fix X"}
	r3, err := ev.Evaluate(context.Background(), reviewGate, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{})
	if err != nil {
		t.Fatalf("Evaluate #3: %v", err)
	}
	if r3.Target != "implement" || r3.Attempt != 1 || r3.Escalated {
		t.Fatalf("r3 = %+v, want target=implement attempt=1 escalated=false", r3)
	}
	if r3.Verdict == nil || r3.Verdict.Summary != "please fix X" {
		t.Fatalf("r3.Verdict = %+v, want the reviewer's verdict attached", r3.Verdict)
	}

	// Reviewer gate: approve -> complete.
	rev.reviewVerdict = apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "looks good"}
	r4, err := ev.Evaluate(context.Background(), reviewGate, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{})
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
	wantAttempts := []int{1, 0, 1, 0}
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
		r, err := ev.Evaluate(context.Background(), g, env, "implement", subject)
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
	ev := &Evaluator{Automated: auto, MaxRepasses: 2, Journal: run, Attempts: map[string]int{"autogate": 2}}

	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "failure"}}
	subject := apiv1.ResultEnvelope{Status: apiv1.ResultFailure}

	r, err := ev.Evaluate(context.Background(), g, env, "implement", subject)
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

	// A fresh, unseeded Evaluator against the identical sequence must NOT
	// escalate on the first call — the control proving the seed above, not
	// MaxRepasses or the fixture, is what drove the escalation.
	freshAuto := &fakeAutomated{outcomes: []string{OutcomeFail}}
	fresh := &Evaluator{Automated: freshAuto, MaxRepasses: 2, Journal: newTestJournal(t)}
	fr, err := fresh.Evaluate(context.Background(), g, env, "implement", subject)
	if err != nil {
		t.Fatalf("Evaluate (fresh): %v", err)
	}
	if fr.Attempt != 1 || fr.Escalated {
		t.Fatalf("Evaluate (fresh) = %+v, want attempt=1 escalated=false", fr)
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
	if _, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "failure"}}, "implement", apiv1.ResultEnvelope{}); err == nil {
		t.Fatal("want error when the outcome has no defined branch (GT-002)")
	}
}

func TestEvaluatorHumanGateNotSupportedAtV0(t *testing.T) {
	g := apiv1.Gate{Name: "approve", Evaluator: apiv1.EvaluatorHuman, Human: &apiv1.HumanGate{}, Branches: map[string]string{"approved": "done"}}
	ev := &Evaluator{}
	if _, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}); err == nil {
		t.Fatal("want error: human gates are not supported at V0")
	}
}

func TestEvaluatorRequiresConfiguredDependency(t *testing.T) {
	autoGate := apiv1.Gate{Name: "g", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "status-equals"}, Branches: map[string]string{OutcomePass: ""}}
	if _, err := (&Evaluator{}).Evaluate(context.Background(), autoGate, apiv1.InvocationEnvelope{}, "s", apiv1.ResultEnvelope{}); err == nil {
		t.Fatal("want error when Automated is not configured")
	}

	agenticGate := apiv1.Gate{Name: "g", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "r"}, Branches: map[string]string{"pass": ""}}
	if _, err := (&Evaluator{}).Evaluate(context.Background(), agenticGate, apiv1.InvocationEnvelope{}, "s", apiv1.ResultEnvelope{}); err == nil {
		t.Fatal("want error when Reviewer is not configured")
	}
}

func TestRecordVerdictNilJournalIsNoop(t *testing.T) {
	if err := recordVerdict(nil, Result{Gate: "g", Outcome: "pass", Target: ""}); err != nil {
		t.Fatalf("recordVerdict with nil Journal: %v", err)
	}
}
