package engine

// Replay and determinism coverage for plan items E4–E9 (#3882).
//
// The parity harness runs both lanes ONCE, in-process, and compares them. That
// is the wrong instrument for the two failure modes this port is most exposed
// to, because neither is a divergence between the lanes — both are divergences
// between the engine and ITSELF:
//
//   - NON-DETERMINISM. The port adds three maps to the walk (lastDiffDigest,
//     addenda, contextRejected) and one derived-from-history helper
//     (projectedEvents, which the remediation-evidence obligation reads).
//     Reading a Go map by key is
//     deterministic; ranging one is not. An edit that starts ranging any of
//     them — to build a message, to grade evidence — reorders the walk's
//     commands on replay and wedges every in-flight run.
//   - HISTORY SKEW. A worker running this code replays histories recorded by a
//     worker that predates it. That is the constraint that put the diff capture
//     and both short-circuits INSIDE ReviewGoober rather than in a new activity
//     (a new ExecuteActivity ahead of an existing one is an immediate
//     nondeterminism error), made GateReviewResult embed apiv1.Verdict so an
//     old bare-verdict payload decodes with every new field zero, and made
//     InvokeGoober's new arguments TRAILING and positional so an old history's
//     shorter argument list still decodes.
//
// TestE4FixturesAreDeterministic covers the first cheaply. TestE4WalkHistory
// Replays covers both against a real dev server, which is the only place a
// genuine replay happens.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// e4Fixture is a fixture exercising one or more of the E4–E9 ports, named so a
// determinism failure names the port that introduced it.
type e4Fixture struct {
	name     string
	spec     apiv1.WorkflowSpec
	script   map[string][]scriptedCall
	verdicts map[string][]apiv1.Verdict
	// diffs is what the gate's workspace reports per attempt. A single entry
	// is reported for every attempt, which is what makes the dedup fixture a
	// dedup fixture.
	diffs map[string][][]byte
	// exercises is the fixture's ANTI-VACUITY assertion: proof that the walk
	// actually took the port's branch.
	//
	// Without it this whole file degrades silently. A determinism test compares
	// a fixture against ITSELF, so a fixture that stopped reaching its port —
	// because a validation rule changed, because a scripted verdict stopped
	// matching — keeps passing while testing nothing at all, and a replay test
	// over a history that never contained the new command sequence proves the
	// same nothing.
	exercises func(events []journal.Event) error
}

// e4ReviewSpec is the shared shape: one agentic implementer under one agentic
// reviewer, with the reviewer's needs-changes branch re-entering the stage.
// That re-entry is what puts the repass state — the diff digest, the addendum,
// the injected episode — on the second pass, where every one of these ports
// lives.
func e4ReviewSpec() apiv1.WorkflowSpec {
	return fixtureSpec("implement",
		[]apiv1.Task{agenticTask("implement", "review")},
		[]apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAgentic,
			Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			Branches: map[string]string{
				"pass":          wf.TerminalComplete,
				"fail":          wf.TargetAbort,
				"needs-changes": "implement",
			},
		}},
	)
}

func e4Fixtures() []e4Fixture {
	needsChanges := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Summary:  "address the finding",
		Findings: []apiv1.Finding{{ID: "f1", Message: "missing test", Severity: apiv1.SeverityError}},
	}
	return []e4Fixture{
		{
			// E4: the walk never dispatches the reviewer, so the history has a
			// GAP where an activity used to be. A replay of it must take the
			// same gap.
			name:      "cached verdict short-circuit",
			exercises: requireGateAnnotation("verdictCacheHit", true),
			spec: fixtureSpec("implement",
				[]apiv1.Task{detTask("implement", "review")},
				[]apiv1.Gate{{
					Name: "review", Evaluator: apiv1.EvaluatorAgentic,
					Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
					Branches: map[string]string{
						"pass": wf.TerminalComplete, "fail": wf.TargetAbort, "needs-changes": "implement",
					},
				}},
			),
			script: map[string][]scriptedCall{
				"implement": {succeed(e4CachedVerdict(apiv1.VerdictPass))},
			},
		},
		{
			// E5, first arm: the diff is read, found empty, and the gate
			// resolves from a synthesized verdict — an activity that returns
			// WITHOUT having called the reviewer, which is a different command
			// sequence from one that did.
			name:      "empty-diff fast-fail",
			exercises: requireGateAnnotation("reason", gate.ReasonRepassBudgetExhausted),
			spec:      e4ReviewSpec(),
			script: map[string][]scriptedCall{
				"implement": {succeed(map[string]interface{}{"claimed": "done"})},
			},
			diffs: map[string][][]byte{"review": {nil}},
		},
		{
			// E5, second arm: the lastDiffDigest MAP carries state across the
			// repass. This is the fixture a range over it would break.
			name:      "duplicate-diff dedup across a repass",
			exercises: requireGateAnnotation("duplicateDiff", true),
			spec:      e4ReviewSpec(),
			script: map[string][]scriptedCall{
				"implement": {
					succeed(map[string]interface{}{"attempt": "1"}),
					succeed(map[string]interface{}{"attempt": "2"}),
				},
			},
			verdicts: map[string][]apiv1.Verdict{"review": {needsChanges}},
			diffs: map[string][][]byte{
				"review": {[]byte("diff --git a/main.go b/main.go\n+// unchanged\n")},
			},
		},
		{
			// E6/E7: a real second review. The repass carries the diff
			// artifact, the "<gate>.diff" pointer, the repass cause, the
			// remediation-evidence obligation and the learning findings — the
			// whole ported context set, all of it derived from the projection
			// rather than from in-memory state, which is precisely what a
			// replay re-derives.
			name: "reviewer diff evidence and repass context",
			exercises: bothExercised(
				requireArtifactRecorded("review/reviewer-diff.patch"),
				requireAnnotationKind(runner.RemediationEvidenceRequiredKind),
			),
			spec: e4ReviewSpec(),
			script: map[string][]scriptedCall{
				"implement": {
					succeed(map[string]interface{}{"attempt": "1"}),
					succeed(map[string]interface{}{"attempt": "2"}),
				},
			},
			verdicts: map[string][]apiv1.Verdict{
				"review": {needsChanges, {Decision: apiv1.VerdictPass, Summary: "resolved"}},
			},
			diffs: map[string][][]byte{
				"review": {
					[]byte("diff --git a/main.go b/main.go\n+// first attempt\n"),
					[]byte("diff --git a/main.go b/main.go\n+// second attempt, with the test\n"),
				},
			},
		},
	}
}

// requireGateAnnotation asserts SOME gate.evaluated carries key=want.
func requireGateAnnotation(key string, want any) func([]journal.Event) error {
	return func(events []journal.Event) error {
		seen := []any{}
		for _, e := range events {
			if e.Type != journal.EventGateEvaluated {
				continue
			}
			if e.Runner[key] == want {
				return nil
			}
			seen = append(seen, e.Runner[key])
		}
		return fmt.Errorf("no gate.evaluated carries %s=%v (saw %v); this fixture no longer reaches the port it exists for",
			key, want, seen)
	}
}

// requireAnnotationKind asserts the walk wrote a runner.annotation of a kind.
func requireAnnotationKind(kind string) func([]journal.Event) error {
	return func(events []journal.Event) error {
		for _, e := range events {
			if e.Type == journal.EventRunnerAnnotation && e.Runner["kind"] == kind {
				return nil
			}
		}
		return fmt.Errorf("no runner.annotation of kind %q; this fixture no longer reaches the port it exists for", kind)
	}
}

// requireArtifactRecorded asserts the walk committed an artifact whose name
// ends in the given suffix.
func requireArtifactRecorded(suffix string) func([]journal.Event) error {
	return func(events []journal.Event) error {
		names := []string{}
		for _, e := range events {
			if e.Type != journal.EventArtifactRecorded {
				continue
			}
			if strings.HasSuffix(e.Name, suffix) {
				return nil
			}
			names = append(names, e.Name)
		}
		return fmt.Errorf("no artifact named %q was recorded (saw %v); this fixture no longer reaches the port it exists for",
			suffix, names)
	}
}

func bothExercised(checks ...func([]journal.Event) error) func([]journal.Event) error {
	return func(events []journal.Event) error {
		for _, check := range checks {
			if err := check(events); err != nil {
				return err
			}
		}
		return nil
	}
}

// e4CachedVerdict is a deterministic subject's digest-matched prior verdict,
// in the scalar-only shape a stage's Outputs actually carry (#523).
func e4CachedVerdict(decision apiv1.VerdictDecision) map[string]interface{} {
	raw, err := json.Marshal(apiv1.Verdict{
		Decision:  decision,
		Rationale: "the sibling run reviewed this identical tree",
	})
	if err != nil {
		panic(fmt.Sprintf("encode cached verdict fixture: %v", err))
	}
	return map[string]interface{}{runner.CachedVerdictOutputKey: string(raw)}
}

// e4Workspaces builds the fixture's workspace fake. A gate absent from the
// fixture's diffs gets a workspace that cannot report one at all, which is the
// pre-#3882 provisioner and must never be read as "the stage changed nothing".
func e4Workspaces(t *testing.T, fx e4Fixture) *fakeWorkspaces {
	t.Helper()
	ws := testWorkspaces(t)
	for gateName, perRead := range fx.diffs {
		ws.scriptDiffSequence(gateName, perRead)
	}
	return ws
}

// e4Walk executes one fixture in the test environment, wiring the two seams
// the E4–E9 ports read: the scripted reviewer and the workspace diff.
func e4Walk(t *testing.T, runID string, fx e4Fixture) ([]journal.Event, RunResult, error) {
	t.Helper()
	exec := newScriptedExec(fx.script)
	exec.verdicts = fx.verdicts
	ws := e4Workspaces(t, fx)
	in := runInput(runID, fx.spec)
	in.TriggerKind = string(journal.TriggerManual)
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	env.RegisterActivity(&Activities{
		Goober: exec, Det: exec, Auto: gate.NewAutomatedEvaluator(), Workspaces: ws,
	})
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

// Two identical walks of an E4–E9 fixture must produce identical journals.
//
// The ports this covers are the ones holding MAP state across a repass
// (lastDiffDigest, addenda, contextRejected) and the ones deriving their
// payload from the projection (the remediation-evidence obligation).
// Both are deterministic today; this is what says so on the first
// edit that stops being true, rather than on a production replay.
func TestE4FixturesAreDeterministic(t *testing.T) {
	for _, fx := range e4Fixtures() {
		t.Run(fx.name, func(t *testing.T) {
			firstEvents, firstResult, firstErr := e4Walk(t, "e4-det", fx)
			secondEvents, secondResult, secondErr := e4Walk(t, "e4-det", fx)
			if (firstErr == nil) != (secondErr == nil) {
				t.Fatalf("walk error differs between runs: %v vs %v", firstErr, secondErr)
			}
			if first, second := terminalOf(firstResult), terminalOf(secondResult); first != second {
				t.Fatalf("terminals differ between two identical walks:\n  first:  %s\n  second: %s", first, second)
			}
			if err := diffConformanceViews(firstEvents, secondEvents); err != nil {
				t.Fatalf("journal projections differ between two identical walks: %v", err)
			}
			assertImplementationAnnotationsStable(t, firstEvents, secondEvents)
			if fx.exercises != nil {
				if err := fx.exercises(firstEvents); err != nil {
					t.Fatalf("%v", err)
				}
			}
		})
	}
}

// assertImplementationAnnotationsStable compares the RUNNER-namespace
// annotations the port writes.
//
// diffConformanceViews cannot see them — runner.* is excluded from conformance
// by prefix — and they are where every counter, digest and derived payload in
// this port lives. A diff digest that drifted between two identical walks, or
// an episode whose ordering changed, would be a genuine replay hazard reported
// by the conformance comparison as nothing at all.
func assertImplementationAnnotationsStable(t *testing.T, first, second []journal.Event) {
	t.Helper()
	firstAnnotations := implementationAnnotations(first)
	secondAnnotations := implementationAnnotations(second)
	if len(firstAnnotations) != len(secondAnnotations) {
		t.Fatalf("annotation count differs between two identical walks: %d vs %d\n  first:  %v\n  second: %v",
			len(firstAnnotations), len(secondAnnotations), firstAnnotations, secondAnnotations)
	}
	for i := range firstAnnotations {
		if firstAnnotations[i] != secondAnnotations[i] {
			t.Fatalf("annotation %d differs between two identical walks:\n  first:  %s\n  second: %s",
				i, firstAnnotations[i], secondAnnotations[i])
		}
	}
}

// implementationAnnotations renders every gate.evaluated Runner map and every
// runner.annotation as a stable string, in journal order.
func implementationAnnotations(events []journal.Event) []string {
	var out []string
	for _, e := range events {
		if e.Type != journal.EventGateEvaluated && e.Type != journal.EventRunnerAnnotation {
			continue
		}
		if len(e.Runner) == 0 {
			continue
		}
		// json.Marshal sorts map keys, so this is a stable rendering of an
		// unstable-by-construction type.
		raw, err := json.Marshal(e.Runner)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: unrenderable: %v", e.Type, err))
			continue
		}
		out = append(out, fmt.Sprintf("%s/%s/%s: %s", e.Type, e.Stage, e.Gate, raw))
	}
	return out
}

// A history recorded by a worker walking the E4–E9 ports must replay cleanly.
//
// This is the test that governs the shape of the whole port. ReviewGoober
// gained fields and arguments rather than a sibling activity, and InvokeGoober
// gained TRAILING arguments, because a replay is what makes any other choice a
// production outage: Temporal compares the command sequence, and an activity
// scheduled in a new position is a mismatch no amount of correct behaviour
// recovers from.
func TestE4WalkHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})
	temporalClient := server.Client()

	for i, fx := range e4Fixtures() {
		t.Run(fx.name, func(t *testing.T) {
			taskQueue := "e4-replay"
			exec := newScriptedExec(fx.script)
			exec.verdicts = fx.verdicts
			ws := e4Workspaces(t, fx)
			w := temporalworker.New(temporalClient, taskQueue, temporalworker.Options{})
			RegisterWith(w, &Activities{
				Goober: exec, Det: exec, Auto: gate.NewAutomatedEvaluator(), Workspaces: ws,
			})
			if err := w.Start(); err != nil {
				t.Fatalf("start Temporal worker: %v", err)
			}
			t.Cleanup(w.Stop)

			in := runInput("e4-replay", fx.spec)
			in.RunID = "e4-replay-" + strconv.Itoa(i)
			in.TriggerKind = string(journal.TriggerManual)
			run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID:        "e4-replay-" + strconv.Itoa(i),
				TaskQueue: taskQueue,
			}, Run, in)
			if err != nil {
				t.Fatalf("execute workflow: %v", err)
			}
			var result RunResult
			// A fixture that ends at @abort or @escalate fails the workflow;
			// the history is recorded either way, and the history is the
			// subject of this test.
			_ = run.Get(ctx, &result)

			iter := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false,
				enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
			history := &historypb.History{}
			for iter.HasNext() {
				event, err := iter.Next()
				if err != nil {
					t.Fatalf("read workflow history: %v", err)
				}
				history.Events = append(history.Events, event)
			}
			replayer := temporalworker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(Run)
			if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
				t.Fatalf("replay E4 workflow history: %v", err)
			}
		})
	}
}
