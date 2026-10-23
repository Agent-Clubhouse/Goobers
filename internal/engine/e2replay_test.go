package engine

// Replay and determinism coverage for plan item E2 (#3874).
//
// Every E2 port adds STATE or a BRANCH to the workflow function, and the
// workflow function is replayed: Temporal re-executes it against a recorded
// history and requires the same command sequence out of it. Two failure modes
// follow, and neither is visible to the parity harness (which runs both sides
// once, in-process):
//
//   - NON-DETERMINISM. The completed-stage map is a Go map. Reading it by key is
//     deterministic; iterating it is not. A future edit that ranges over it —
//     to build an error message, to grade inputs — would produce a different
//     command order on replay and wedge the run.
//   - HISTORY SKEW. A worker running the new code replays a history recorded by
//     one running the old code. RunResult.NoWork is `omitempty` for this reason,
//     and the retry-decision annotation had to be added to projectableEventTypes
//     for the same one.
//
// TestE2FixturesAreDeterministic covers the first cheaply, in the test
// environment. TestE2WalkHistoryReplays covers both against a real dev server,
// which is the only place a genuine replay happens.

import (
	"context"
	"io"
	"strconv"
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

// e2Fixture is a fixture exercising one or more of the E2 ports, named so a
// determinism failure says which port introduced the nondeterminism.
type e2Fixture struct {
	name   string
	spec   apiv1.WorkflowSpec
	script map[string][]scriptedCall
}

// e2Fixtures covers all four ports: stage-qualified inputsFrom (the map),
// the retry-decision annotation and its knownOutcome shortcut (the branch),
// the #415 escalation bypass (the other branch), and NoWork.
func e2Fixtures() []e2Fixture {
	return []e2Fixture{
		{
			name: "stage-qualified inputsFrom",
			spec: fixtureSpec("produce", []apiv1.Task{
				{Name: "produce", Type: apiv1.TaskDeterministic, Goal: "produce",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: "intervene"},
				{Name: "intervene", Type: apiv1.TaskDeterministic, Goal: "intervene",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: "consume"},
				{Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume",
					Run:        &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					InputsFrom: map[string]string{"resolved": "produce.selectedNumber"},
					Next:       wf.TerminalComplete},
			}, nil),
			script: map[string][]scriptedCall{
				"produce":   {succeed(map[string]interface{}{"selectedNumber": "4242"})},
				"intervene": {succeed(map[string]interface{}{"note": "unrelated"})},
			},
		},
		{
			name: "retry decision and knownOutcome shortcut",
			spec: fixtureSpec("implement",
				[]apiv1.Task{detTask("implement", "review")},
				[]apiv1.Gate{statusGate("review", map[string]string{
					"pass": wf.TerminalComplete, "fail": "implement",
				})}),
			script: map[string][]scriptedCall{
				"implement": {
					fail("nonzero_exit", "3 tests failed"),
					fail(runner.BaseSyncConflictErrorCode, "base moved"),
					succeed(map[string]interface{}{"tests": "green"}),
				},
			},
		},
		{
			name: "non-retryable escalation bypass",
			spec: fixtureSpec("implement", []apiv1.Task{
				detTask("implement", "review"),
				detTask("park-escalated", wf.TargetEscalate),
			}, []apiv1.Gate{statusGate("review", map[string]string{
				"pass": wf.TerminalComplete, "fail": "implement", "escalate": "park-escalated",
			})}),
			script: map[string][]scriptedCall{
				"implement":      {escalateFailure("ISSUE_OVER_SCOPE")},
				"park-escalated": {succeed(map[string]interface{}{"parked": "true"})},
			},
		},
		{
			name: "no-work short circuit",
			spec: fixtureSpec("poll", []apiv1.Task{detTask("poll", wf.TerminalComplete)}, nil),
			script: map[string][]scriptedCall{
				"poll": {{result: apiv1.ResultEnvelope{Status: apiv1.ResultNoWork}}},
			},
		},
	}
}

// Each E2 fixture must produce the identical result and the identical journal
// projection when walked twice from the same inputs.
//
// This is the cheap guard on the completed-stage map. The map is only ever
// READ BY KEY today, which is deterministic — but nothing in the type system
// says so, and a range over it in a future error message or grading pass would
// reorder the walk's commands on replay. Running the whole fixture set twice
// and requiring byte-identical journals catches that on the first edit that
// introduces it, rather than on a production replay.
func TestE2FixturesAreDeterministic(t *testing.T) {
	for _, fx := range e2Fixtures() {
		t.Run(fx.name, func(t *testing.T) {
			firstEvents, firstResult, firstErr := shortcutRunWithID(t, "det-1", fx.spec, fx.script)
			secondEvents, secondResult, secondErr := shortcutRunWithID(t, "det-1", fx.spec, fx.script)
			if (firstErr == nil) != (secondErr == nil) {
				t.Fatalf("walk error differs between runs: %v vs %v", firstErr, secondErr)
			}
			// RunResult carries an Outputs map, so compare the terminal the
			// daemon's Starter actually reads rather than the whole struct.
			if first, second := terminalOf(firstResult), terminalOf(secondResult); first != second {
				t.Fatalf("terminals differ between two identical walks:\n  first:  %s\n  second: %s", first, second)
			}
			if err := diffConformanceViews(firstEvents, secondEvents); err != nil {
				t.Fatalf("journal projections differ between two identical walks: %v", err)
			}
			assertRetryDecisionsStable(t, firstEvents, secondEvents)
		})
	}
}

// assertRetryDecisionsStable compares the non-normative retry-decision
// annotations too. diffConformanceViews cannot see them (runner.* is excluded
// by prefix), and they are the one E2 surface carrying a counter — an attempt
// number that drifted between runs would be a real replay hazard reported as
// nothing at all.
func assertRetryDecisionsStable(t *testing.T, first, second []journal.Event) {
	t.Helper()
	firstDecisions := retryDecisions(paritySide{Name: "first", Events: first})
	secondDecisions := retryDecisions(paritySide{Name: "second", Events: second})
	if err := diffRetryDecisions("second walk", secondDecisions, firstDecisions); err != nil {
		t.Fatalf("retry-decision annotations differ between two identical walks: %v", err)
	}
}

// A history recorded by a worker walking the E2 ports must replay cleanly.
//
// This is the test that would have caught the projection gap the annotation
// introduced: runner.annotation was not in projectableEventTypes, so a history
// containing one failed its journal projection closed — on every run that took
// a gate's fail branch, which is the implementation lane's entire repass loop.
//
// It runs against a real dev server because the test environment does not
// replay: it executes the workflow once and hands back the result. Only
// NewWorkflowReplayer over a recorded history re-runs the workflow function
// against Temporal's own determinism check.
func TestE2WalkHistoryReplays(t *testing.T) {
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

	for i, fx := range e2Fixtures() {
		t.Run(fx.name, func(t *testing.T) {
			taskQueue := "e2-replay"
			exec := newScriptedExec(fx.script)
			w := temporalworker.New(temporalClient, taskQueue, temporalworker.Options{})
			RegisterWith(w, &Activities{
				Goober:     exec,
				Det:        exec,
				Auto:       gate.NewAutomatedEvaluator(),
				Workspaces: testWorkspaces(t),
			})
			if err := w.Start(); err != nil {
				t.Fatalf("start Temporal worker: %v", err)
			}
			t.Cleanup(w.Stop)

			in := runInput("e2-replay", fx.spec)
			in.RunID = "e2-replay-" + strconv.Itoa(i)
			in.TriggerKind = string(journal.TriggerManual)
			run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID:        "e2-replay-" + strconv.Itoa(i),
				TaskQueue: taskQueue,
			}, Run, in)
			if err != nil {
				t.Fatalf("execute workflow: %v", err)
			}
			var result RunResult
			if err := run.Get(ctx, &result); err != nil {
				t.Fatalf("workflow result: %v", err)
			}

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
				t.Fatalf("replay E2 workflow history: %v", err)
			}
		})
	}
}

// terminalOf reduces a RunResult to the comparable terminal the scheduler and
// the parity harness both read.
func terminalOf(res RunResult) parityTerminal {
	return parityTerminal{
		Status:      res.Status,
		FinalState:  res.FinalState,
		FailureCode: res.FailureCode,
		NoWork:      res.NoWork,
	}
}
