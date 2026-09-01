package engine

// The knownOutcome shortcut and the retry-decision annotation on the engine
// walk — plan item E2 (#3874).
//
// Neither is visible on the parity harness's four surfaces. The shortcut is
// invisible because the automated gate evaluator is not routed through
// recordingExec on either side, so "did the checker dispatch?" never reaches an
// envelope; the annotation is invisible because journal.IsConformanceNormative
// excludes the whole runner.* namespace by prefix. The parity rows pin their
// CONSEQUENCES (the routing, and the annotation compared on the raw event log);
// these tests pin the mechanisms directly, with a counting evaluator.

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// refusingAutomated fails any dispatch. It is the sharper instrument: a
// shortcut that silently stopped applying would still produce the right outcome
// with a counting evaluator (the real checker agrees with the shortcut, which
// is why the shortcut is sound), so counting alone can only catch the
// regression if the count is asserted exactly. Refusing turns a lost shortcut
// into a failed run.
type refusingAutomated struct{ t *testing.T }

func (r *refusingAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	return "", errors.New("the gate evaluator must not be dispatched for a failure whose status-equals outcome is already known")
}

// shortcutRun executes the fixture through the engine with the supplied
// evaluator and returns the projected journal.
func shortcutRun(t *testing.T, auto invoke.Automated, spec apiv1.WorkflowSpec, script map[string][]scriptedCall) ([]journal.Event, RunResult, error) {
	t.Helper()
	return engineWalk(t, "shortcut-1", auto, spec, script)
}

// shortcutRunWithID is engineWalk with the package's real automated evaluator,
// for tests that care about the walk rather than about dispatch counting.
func shortcutRunWithID(t *testing.T, runID string, spec apiv1.WorkflowSpec, script map[string][]scriptedCall) ([]journal.Event, RunResult, error) {
	t.Helper()
	return engineWalk(t, runID, gate.NewAutomatedEvaluator(), spec, script)
}

// engineWalk executes one fixture through the engine in Temporal's test
// environment and returns its projected journal, result and workflow error.
func engineWalk(t *testing.T, runID string, auto invoke.Automated, spec apiv1.WorkflowSpec, script map[string][]scriptedCall) ([]journal.Event, RunResult, error) {
	t.Helper()
	exec := newScriptedExec(script)
	in := RunInput{
		RunID:                  runID,
		Gaggle:                 "web",
		WorkflowName:           "shortcut",
		Version:                1,
		PreviewFeaturesEnabled: boolPointer(true),
		Spec:                   spec,
		RepoRef:                apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		TriggerKind:            string(journal.TriggerManual),
	}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	env.RegisterActivity(&Activities{Goober: exec, Det: exec, Auto: auto, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(Run, in)
	workflowErr := env.GetWorkflowError()
	var res RunResult
	if workflowErr == nil {
		if err := env.GetWorkflowResult(&res); err != nil {
			t.Fatalf("engine result: %v", err)
		}
	}
	return projectEngineJournal(t, env), res, workflowErr
}

// shortcutSpec is one stage under a status-equals gate whose fail branch parks.
func shortcutSpec() apiv1.WorkflowSpec {
	return fixtureSpec("build",
		[]apiv1.Task{detTask("build", "check"), detTask("park", wf.TargetAbort)},
		[]apiv1.Gate{statusGate("check", map[string]string{"pass": wf.TerminalComplete, "fail": "park"})},
	)
}

// A status-equals gate standing over a nonzero_exit or base_sync_conflict
// failure must resolve WITHOUT dispatching the checker: the check compares a
// status the walk is already holding, so the answer is known and the activity
// is pure cost. This is the local runner's retryFailureClass shortcut, and it
// is shared rather than mirrored so the two runners cannot disagree about which
// codes are shortcut-able.
func TestKnownOutcomeShortcutSkipsTheChecker(t *testing.T) {
	for _, code := range []string{"nonzero_exit", runner.BaseSyncConflictErrorCode} {
		t.Run(code, func(t *testing.T) {
			events, res, err := shortcutRun(t, &refusingAutomated{t: t}, shortcutSpec(),
				map[string][]scriptedCall{"build": {fail(code, "the command failed")}})
			if err != nil {
				t.Fatalf("workflow error = %v; the shortcut must resolve this gate without an evaluator", err)
			}
			if res.Status != StatusBlocked || res.FinalState != "park" {
				t.Fatalf("result = %+v, want the fail branch to park — the shortcut must produce the same outcome "+
					"the checker would", res)
			}
			// The shortcut is not a silent path: it journals the same gate
			// markers a dispatched evaluation does, because a run whose gate
			// left no started/evaluated pair is unreadable to everything
			// downstream of the journal.
			assertGateMarkers(t, events, "check")
		})
	}
}

// The negative half. A failure the shortcut does not recognize, and a SUCCESS,
// must both still dispatch the checker — otherwise the shortcut would be
// deciding gates it has no basis to decide.
func TestUnrecognizedOutcomesStillDispatchTheChecker(t *testing.T) {
	cases := map[string]map[string][]scriptedCall{
		"unrecognized failure code": {"build": {fail("assertion_failed", "an assertion tripped")}},
		"success":                   {"build": {succeed(map[string]interface{}{"sha": "abc"})}},
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			counter := &countingAutomated{inner: gate.NewAutomatedEvaluator()}
			if _, _, err := shortcutRun(t, counter, shortcutSpec(), script); err != nil {
				t.Fatalf("workflow error = %v", err)
			}
			if counter.count() != 1 {
				t.Errorf("evaluator dispatched %d time(s), want 1 — the shortcut must not decide an outcome it "+
					"cannot know", counter.count())
			}
		})
	}
}

// A non-status-equals automated gate is never shortcut, whatever the failure
// code. The shortcut's soundness argument is entirely about what status-equals
// compares; any other check has its own logic and must be asked.
func TestShortcutOnlyAppliesToStatusEqualsGates(t *testing.T) {
	spec := fixtureSpec("build",
		[]apiv1.Task{detTask("build", "check"), detTask("park", wf.TargetAbort)},
		[]apiv1.Gate{{
			Name:      "check",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "failure-class"},
			Branches:  map[string]string{"pass": wf.TerminalComplete, "fail": "park", "infra": "park"},
		}},
	)
	counter := &countingAutomated{inner: gate.NewAutomatedEvaluator()}
	if _, _, err := shortcutRun(t, counter, spec,
		map[string][]scriptedCall{"build": {fail("nonzero_exit", "the command failed")}}); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	if counter.count() != 1 {
		t.Errorf("evaluator dispatched %d time(s), want 1 — only status-equals has a knowable outcome",
			counter.count())
	}
}

// assertGateMarkers requires the gate's paused/started/evaluated markers, in
// that order. The shortcut resolves the outcome in-process, so it would be easy
// to write one that emits nothing; a gate that left no trace in the journal is
// invisible to the read service, to conformance, and to replay.
func assertGateMarkers(t *testing.T, events []journal.Event, gateName string) {
	t.Helper()
	want := []journal.EventType{journal.EventGatePaused, journal.EventGateStarted, journal.EventGateEvaluated}
	got := make([]journal.EventType, 0, len(want))
	for _, e := range events {
		if e.Gate != gateName {
			continue
		}
		switch e.Type {
		case journal.EventGatePaused, journal.EventGateStarted, journal.EventGateEvaluated:
			got = append(got, e.Type)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("gate %q left markers %v, want %v — a shortcut gate must journal exactly what a dispatched one does",
			gateName, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("gate %q markers = %v, want %v", gateName, got, want)
		}
	}
}
