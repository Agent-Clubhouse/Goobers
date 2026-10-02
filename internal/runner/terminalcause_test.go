package runner

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func terminalCauseRun(t *testing.T, machine *workflow.Machine) *journal.Run {
	t.Helper()
	data, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "terminal-cause", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version, WorkflowDigest: machine.Digest()}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: data}, journal.WithInputIntegrity(map[string]apiv1.Integrity{journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return run
}
func readTerminalCause(t *testing.T, dir string) *journal.TerminalCause {
	t.Helper()
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	cause, err := rd.TerminalCause()
	if err != nil {
		t.Fatal(err)
	}
	return cause
}
func appendCauseEvent(t *testing.T, jr *journal.Run, event journal.Event) {
	t.Helper()
	if err := jr.Append(event); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalCauseGateAndAbort(t *testing.T) {
	for _, tc := range []struct {
		name, target, reason string
		phase                journal.RunPhase
		class                journal.TerminalClassification
		attempt              int
	}{
		{"abort", workflow.TargetAbort, "", journal.PhaseAborted, journal.TerminalDefinedAbort, 0},
		{"escalate", workflow.TargetEscalate, "", journal.PhaseEscalated, journal.TerminalEscalation, 0},
		{"budget", workflow.TargetEscalate, gate.ReasonRepassBudgetExhausted, journal.PhaseEscalated, journal.TerminalPolicyExhaustion, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jr := terminalCauseRun(t, fixtureMachine(t))
			verdict, _ := json.Marshal(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: "specific human explanation"})
			ref, err := jr.RecordArtifact("verdict", verdict)
			if err != nil {
				t.Fatal(err)
			}
			appendCauseEvent(t, jr, journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: tc.target, Ref: &ref, Runner: map[string]any{"reason": tc.reason, "repassAttempt": tc.attempt}})
			seq := jr.Seq()
			r := &Runner{cfg: Config{PrepareTerminal: func(_ string, _ journal.RunPhase, run *journal.Run) error {
				return run.Append(journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "cleanup_failed", Message: "unrelated"}})
			}, FinalizeTerminal: func(_ string, _ journal.RunPhase) error {
				c := readTerminalCause(t, jr.Dir())
				if c.CausalEventSeq != seq {
					t.Fatalf("finalizer cause=%+v", c)
				}
				return nil
			}}}
			if _, err := r.finish("terminal-cause", jr, tc.phase, "review", 1); err != nil {
				t.Fatal(err)
			}
			c := readTerminalCause(t, jr.Dir())
			if c.Classification != tc.class || c.Target != tc.target || c.Verdict != "fail" || c.Selector != "review" || c.Message != "specific human explanation" || c.CausalEventSeq != seq {
				t.Fatalf("cause=%+v", c)
			}
			if c.Repass == nil || c.Repass.Allowed != 3 || c.Repass.Consumed != min(tc.attempt, 3) {
				t.Fatalf("repass=%+v", c.Repass)
			}
		})
	}
}

func TestTerminalCauseInfrastructureSupersedesStageFailure(t *testing.T) {
	jr := terminalCauseRun(t, fixtureMachine(t))
	appendCauseEvent(t, jr, journal.Event{Type: journal.EventStageFinished, Stage: "implement", Status: "failure", Error: &journal.ErrorDetail{Code: "WORK_FAILED", Message: "old work failure"}})
	r := &Runner{}
	_, err := r.failTerminal(context.Background(), "terminal-cause", jr, apiv1.RepoRef{}, "review", 1, errors.New("gate transport unavailable"))
	if err == nil {
		t.Fatal("expected infrastructure error")
	}
	c := readTerminalCause(t, jr.Dir())
	if c.Classification != journal.TerminalInfrastructureFailure || c.SelectorKind != "condition" || c.Message != "gate transport unavailable" || c.Code != "run_failed" || c.CausalEventSeq != 3 {
		t.Fatalf("cause=%+v", c)
	}
}

func TestTerminalCauseOrdinaryFailureIsNotRetryExhaustion(t *testing.T) {
	jr := terminalCauseRun(t, retryFixtureMachine(t, 3))
	appendCauseEvent(t, jr, journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendCauseEvent(t, jr, journal.Event{Type: journal.EventStageFinished, Stage: "implement", Status: "failure", Attempt: 1})
	r := &Runner{}
	if _, err := r.finishStageFailure(context.Background(), "terminal-cause", jr, apiv1.RepoRef{}, "implement", 1, &apiv1.ErrorInfo{Code: "WORK_FAILED", Message: "not retryable", Retryable: false}); err != nil {
		t.Fatal(err)
	}
	c := readTerminalCause(t, jr.Dir())
	if c.Classification != journal.TerminalStageFailure || c.Code != "WORK_FAILED" || c.CausalEventSeq != 4 || !reflect.DeepEqual(c.Retry, &journal.TerminalBudget{Consumed: 0, Allowed: 2}) {
		t.Fatalf("cause=%+v retry=%+v", c, c.Retry)
	}
}

func TestTerminalCauseDispatchRetryExhaustion(t *testing.T) {
	executor := &flakyDeterministic{failUntil: 10}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return executor, nil }, nil)
	_, err := r.Start(context.Background(), StartInput{RunID: "cause-retry", Machine: retryFixtureMachine(t, 3), RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}})
	if err == nil {
		t.Fatal("expected dispatch failure")
	}
	c := readTerminalCause(t, filepath.Join(runsDir, "cause-retry"))
	if c.Classification != journal.TerminalRetryExhaustion || c.Selector != "implement" || !reflect.DeepEqual(c.Retry, &journal.TerminalBudget{Consumed: 2, Allowed: 2}) {
		t.Fatalf("cause=%+v retry=%+v", c, c.Retry)
	}
}

func TestTerminalCauseOperatorAndResumeRefusal(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(map[bool]string{false: "operator", true: "refusal"}[refusal], func(t *testing.T) {
			jr := terminalCauseRun(t, fixtureMachine(t))
			r := &Runner{}
			want := journal.TerminalOperatorAbort
			if refusal {
				want = journal.TerminalResumeRefused
				if _, err := r.refuseResume(jr, "terminal-cause", "resume_refused_digest_mismatch", "WF-016: changed definition"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := r.finishStalled("terminal-cause", jr, "implement", 1, stalledRequest{kind: interruptCancel, phase: journal.PhaseAborted}); err != nil {
					t.Fatal(err)
				}
			}
			c := readTerminalCause(t, jr.Dir())
			if c.Classification != want || c.CausalEventSeq == 0 || c.Message == "" {
				t.Fatalf("cause=%+v", c)
			}
		})
	}
}

func TestTerminalCauseRecoverGateCrashExactlyOnce(t *testing.T) {
	jr := terminalCauseRun(t, fixtureMachine(t))
	appendCauseEvent(t, jr, journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetAbort})
	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{}
	for i := 0; i < 2; i++ {
		res, done, err := r.resumeTerminalPhase(rd, jr, ResumeInput{RunID: "terminal-cause"})
		if err != nil || !done || res.Phase != journal.PhaseAborted {
			t.Fatalf("resume=%+v %v %v", res, done, err)
		}
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Type == journal.EventRunFinished {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("terminal count=%d", count)
	}
	c := readTerminalCause(t, jr.Dir())
	if c.CausalEventSeq != 2 || c.Target != workflow.TargetAbort {
		t.Fatalf("cause=%+v", c)
	}
}

func TestTerminalCausePollingUsesDedicatedBudget(t *testing.T) {
	def := fixtureMachine(t).Def
	def.Spec.Gates[0].Automated.MaxTimeoutPolls = 7
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	jr := terminalCauseRun(t, machine)
	appendCauseEvent(t, jr, journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "timeout", Target: workflow.TargetEscalate, Runner: map[string]any{"reason": gate.ReasonPollingBudgetExhausted, "repassAttempt": 0, "pollAttempt": 8}})
	if _, err := (&Runner{}).finish("terminal-cause", jr, journal.PhaseEscalated, "review", 1); err != nil {
		t.Fatal(err)
	}
	c := readTerminalCause(t, jr.Dir())
	if !reflect.DeepEqual(c.Poll, &journal.TerminalBudget{Consumed: 7, Allowed: 7}) || c.Repass == nil || c.Repass.Consumed != 0 {
		t.Fatalf("cause=%+v poll=%+v repass=%+v", c, c.Poll, c.Repass)
	}
}

func TestTerminalCauseParallelFailureAttribution(t *testing.T) {
	for _, phase := range []journal.RunPhase{journal.PhaseAborted, journal.PhaseEscalated} {
		t.Run(string(phase), func(t *testing.T) {
			jr := terminalCauseRun(t, fixtureMachine(t))
			target := workflow.TargetAbort
			if phase == journal.PhaseEscalated {
				target = workflow.TargetEscalate
			}
			appendCauseEvent(t, jr, journal.Event{Type: journal.EventParallelFinished, Parallel: "checks", Target: target})
			seq := jr.Seq()
			if _, err := (&Runner{}).finish("terminal-cause", jr, phase, "checks", 2); err != nil {
				t.Fatal(err)
			}
			c := readTerminalCause(t, jr.Dir())
			if c.Selector != "checks" || c.SelectorKind != "condition" || c.CausalEventSeq != seq || c.Target != target {
				t.Fatalf("cause=%+v", c)
			}
		})
	}
}

func TestTerminalCauseGateOverrideKeepsHumanRationale(t *testing.T) {
	jr := terminalCauseRun(t, fixtureMachine(t))
	appendCauseEvent(t, jr, journal.Event{Type: journal.EventGateOverridden, Gate: "review", Verdict: "fail", Target: workflow.TargetAbort, Rationale: "operator inspected the evidence"})
	seq := jr.Seq()
	if _, err := (&Runner{}).finish("terminal-cause", jr, journal.PhaseAborted, "review", 1); err != nil {
		t.Fatal(err)
	}
	c := readTerminalCause(t, jr.Dir())
	if c.Message != "operator inspected the evidence" || c.CausalEventSeq != seq {
		t.Fatalf("cause=%+v", c)
	}
}

func TestTerminalCauseRecoveryStillRefusesStaleHumanDecision(t *testing.T) {
	jr := terminalCauseRun(t, fixtureMachine(t))
	appendCauseEvent(t, jr, journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetAbort})
	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		t.Fatal(err)
	}
	res, done, err := (&Runner{}).resumeTerminalPhase(rd, jr, ResumeInput{RunID: "terminal-cause", HumanDecision: &HumanGateDecision{Gate: "review", Decision: "pass"}})
	if err == nil || !done || res.Phase != journal.PhaseAborted {
		t.Fatalf("resume=%+v %v %v", res, done, err)
	}
	c := readTerminalCause(t, jr.Dir())
	if c.Target != workflow.TargetAbort {
		t.Fatalf("stale decision changed target: %+v", c)
	}
}
