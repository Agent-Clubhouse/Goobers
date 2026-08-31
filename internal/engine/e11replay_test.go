package engine

// Replay and determinism coverage for the E11 infrastructure repass budget
// (#3930).
//
// The port adds STATE to the workflow function: four counter maps instead of
// two, carried through the whole walk as gate.RepassBudget. The workflow
// function is REPLAYED — Temporal re-executes it against a recorded history and
// requires the same command sequence out of it — so state that is not a pure
// function of the recorded outcome sequence is not a failing test, it is a
// wedged production run. Two hazards follow, and the parity harness (which runs
// both sides once, in-process) sees neither:
//
//   - NON-DETERMINISM. Every counter is a Go map. Reading one by key is
//     deterministic; ranging over one is not. gate.RepassBudget.Charge only
//     ever reads and writes by key, and TestRepassBudgetChargeIsDeterministic
//     pins that at the helper — this file pins it through the workflow, where
//     an accidental range in a future error message or grading pass would
//     reorder the walk's commands.
//   - HISTORY SKEW. A worker running this code replays a history recorded by
//     one running the old code. The budget lives entirely in workflow-local
//     state and is never carried on RunInput or an activity payload, so the
//     counters a replay rebuilds come from re-executing the same outcomes:
//     nothing has to be decoded, and a history recorded before the
//     infrastructure counters existed rebuilds them at their zero values, which
//     is what "no infrastructure repass has been charged" means (see
//     TestRepassBudgetIsZeroValueUsableForOldHistories).

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
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// e11Fixture is a fixture over the shipped `local-gate` shape
// (implementation.yaml:418-427), named so a determinism failure says which
// sequence introduced it.
type e11Fixture struct {
	name   string
	spec   apiv1.WorkflowSpec
	script map[string][]scriptedCall
}

func e11LocalGateSpec() apiv1.WorkflowSpec {
	return fixtureSpec("implement",
		[]apiv1.Task{
			detTask("implement", "local-ci"),
			detTask("local-ci", "local-gate"),
			detTask("park-escalated", wf.TargetEscalate),
		},
		[]apiv1.Gate{failureClassGate("local-gate", map[string]string{
			"pass":            wf.TerminalComplete,
			"fail":            "implement",
			"infra":           "local-ci",
			wf.BranchEscalate: "park-escalated",
		})},
	)
}

func e11InfraFailure() scriptedCall {
	return failRetryable("worktree_provision_failed", "runner host contention")
}

// e11Fixtures covers the three sequences that move different counters: an
// infrastructure-only run (the infrastructure budget alone), a mixed run (both
// budgets plus the cross-resets), and a run that recovers (a pass, which clears
// both per-gate counters and leaves the per-target budgets standing).
func e11Fixtures() []e11Fixture {
	return []e11Fixture{
		{
			name: "infrastructure-only sequence",
			spec: e11LocalGateSpec(),
			script: map[string][]scriptedCall{
				"implement":      {succeed(map[string]interface{}{"committed": "true"})},
				"local-ci":       {e11InfraFailure(), e11InfraFailure(), e11InfraFailure(), e11InfraFailure()},
				"park-escalated": {succeed(map[string]interface{}{"parked": "true"})},
			},
		},
		{
			name: "mixed infrastructure and content repasses",
			spec: e11LocalGateSpec(),
			script: map[string][]scriptedCall{
				"implement": {
					succeed(map[string]interface{}{"committed": "true"}),
					succeed(map[string]interface{}{"committed": "true"}),
				},
				"local-ci": {
					e11InfraFailure(),
					e11InfraFailure(),
					fail("nonzero_exit", "unit tests failed"),
					e11InfraFailure(),
					e11InfraFailure(),
					e11InfraFailure(),
					e11InfraFailure(),
				},
				"park-escalated": {succeed(map[string]interface{}{"parked": "true"})},
			},
		},
		{
			name: "infrastructure retries then a pass",
			spec: e11LocalGateSpec(),
			script: map[string][]scriptedCall{
				"implement": {succeed(map[string]interface{}{"committed": "true"})},
				"local-ci": {
					e11InfraFailure(),
					succeed(map[string]interface{}{"ci": "green"}),
				},
			},
		},
	}
}

// Each E11 fixture must produce the identical result, the identical journal
// projection, AND the identical repass accounting when walked twice from the
// same inputs.
func TestE11InfrastructureBudgetIsDeterministic(t *testing.T) {
	for _, fx := range e11Fixtures() {
		t.Run(fx.name, func(t *testing.T) {
			firstEvents, firstResult, firstErr := shortcutRunWithID(t, "e11-det", fx.spec, fx.script)
			secondEvents, secondResult, secondErr := shortcutRunWithID(t, "e11-det", fx.spec, fx.script)
			if (firstErr == nil) != (secondErr == nil) {
				t.Fatalf("walk error differs between runs: %v vs %v", firstErr, secondErr)
			}
			if first, second := terminalOf(firstResult), terminalOf(secondResult); first != second {
				t.Fatalf("terminals differ between two identical walks:\n  first:  %s\n  second: %s", first, second)
			}
			if err := diffConformanceViews(firstEvents, secondEvents); err != nil {
				t.Fatalf("journal projections differ between two identical walks: %v", err)
			}
			// The counters themselves. diffConformanceViews cannot see them
			// (the whole Runner namespace is excluded by prefix), and they are
			// the surface this port added — a repass attempt that drifted
			// between two identical walks would be a real replay hazard
			// reported as nothing at all.
			first := gateEvaluations(paritySide{Name: "first", Events: firstEvents})
			second := gateEvaluations(paritySide{Name: "second", Events: secondEvents})
			if err := diffGateEvaluations("second walk", second, first); err != nil {
				t.Fatalf("repass accounting differs between two identical walks: %v", err)
			}
		})
	}
}

// The engine's in-memory accounting and the accounting its JOURNAL preserves
// must be the same thing.
//
// The budget is workflow-local state: a replaying worker rebuilds it by
// re-executing the recorded outcomes, and the daemon's resume/repair path and
// the local runner's resume both rebuild it from the journal instead. If the
// engine charged correctly but journaled a number that cannot be re-keyed —
// a repassAttempt with no repassTarget, an infrastructure attempt recorded as a
// policy one — the two reconstructions part company at the first crash rather
// than at the next release.
func TestE11JournaledBudgetRebuildsFromTheJournal(t *testing.T) {
	fx := e11Fixtures()[1]
	events, _, _ := shortcutRunWithID(t, "e11-rebuild", fx.spec, fx.script)
	side := paritySide{Name: "engine", Events: events}
	budget, err := rebuildRepassBudget(side)
	if err != nil {
		t.Fatalf("rebuild budget from the engine journal: %v", err)
	}
	if got := budget.InfrastructureRepassAttempts["local-ci"]; got != gate.DefaultMaxInfrastructureRepasses+1 {
		t.Fatalf("infrastructure budget rebuilt from the journal = %d, want %d",
			got, gate.DefaultMaxInfrastructureRepasses+1)
	}
	if got := budget.RepassAttempts["implement"]; got != 1 {
		t.Fatalf("policy budget rebuilt from the journal = %d, want 1 — the content failure charged implement "+
			"exactly once and the infrastructure retries charged it not at all", got)
	}
	if got := budget.RepassAttempts["local-ci"]; got != 0 {
		t.Fatalf("policy budget for local-ci rebuilt from the journal = %d, want 0", got)
	}
}

// A history recorded by a worker walking the E11 port must replay cleanly.
//
// It runs against a real dev server because the test environment does not
// replay: it executes the workflow once and hands back the result. Only
// NewWorkflowReplayer over a recorded history re-runs the workflow function
// against Temporal's own determinism check.
func TestE11WalkHistoryReplays(t *testing.T) {
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

	for i, fx := range e11Fixtures() {
		t.Run(fx.name, func(t *testing.T) {
			taskQueue := "e11-replay"
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

			in := runInput("e11-replay", fx.spec)
			in.RunID = "e11-replay-" + strconv.Itoa(i)
			in.TriggerKind = string(journal.TriggerManual)
			run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID:        "e11-replay-" + strconv.Itoa(i),
				TaskQueue: taskQueue,
			}, Run, in)
			if err != nil {
				t.Fatalf("execute workflow: %v", err)
			}
			var result RunResult
			// A fixture that exhausts its budget ends at @escalate and fails
			// the workflow; the history is recorded either way, and the
			// history is the subject of this test.
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
				t.Fatalf("replay E11 workflow history: %v", err)
			}
		})
	}
}
