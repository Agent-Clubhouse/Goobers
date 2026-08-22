package e2e

// TestEngineStartAndProjectRoundTrip is the #2903 acceptance path: it starts
// an engine run, lets it complete, and projects its journal — proving
// engine-start and engine-project are connected end to end through a
// supported caller, hermetically (no live Temporal server), so it can run as
// part of `make ci`.
//
// The run is built exactly the way cmd/goobers/enginestart.go builds one —
// loading the config-as-code fixture with instance.LoadConfigDir (the same
// loader `loadConfigDirectory` resolves to) and then handing it to
// bootstrap.RegisterGaggleWorkflows, the shared constructor engine-start
// itself calls, for a Registry-pinned RunInput. Its journal is projected the
// same way cmd/goobers/engineproject.go projects one: engine.
// ProjectCompletedRunForGaggle against the standard projectionQuerier shape.
// The only substitution is the Temporal transport: the SDK's in-process test
// workflow environment stands in for a live server for the engine-start
// half. temporaltest.ProjectionQuerier (#2903) is the connective piece: it
// adapts that same test environment to the query shape the completed-run
// projection half expects, so both halves run through their real,
// unmodified production code in one process.

import (
	"context"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestEngineStartAndProjectRoundTrip(t *testing.T) {
	set, _, err := instance.LoadConfigDir("../fixtures/e2e/walking-skeleton")
	if err != nil {
		t.Fatalf("load config dir: %v", err)
	}
	if len(set.Gaggles) == 0 || len(set.Workflows) == 0 {
		t.Fatalf("expected gaggles + workflows in fixture, got %d gaggles, %d workflows", len(set.Gaggles), len(set.Workflows))
	}
	gaggle := set.Gaggles[0]
	workflowName := set.Workflows[0].Name

	reg, project, err := bootstrap.RegisterGaggleWorkflows(set, gaggle.Name)
	if err != nil {
		t.Fatalf("register gaggle workflows: %v", err)
	}

	in, err := reg.StartInput(workflowName, engine.StartSpec{
		RunID:       engine.RunID(gaggle.Name, workflowName, "engine-start-project-roundtrip"),
		Gaggle:      gaggle.Name,
		RepoRef:     project,
		TriggerKind: "manual",
	})
	if err != nil {
		t.Fatalf("start input: %v", err)
	}

	// engine-start half: run the pinned RunInput to completion on the
	// Temporal SDK test environment, exactly as engine.TemporalStarter would
	// dispatch it onto a live task queue, minus the network.
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&engine.Activities{
		Goober:     fakeGoober{},
		Workspaces: tempWorkspaces{t: t},
	})
	env.ExecuteWorkflow(engine.Run, in)

	if !env.IsWorkflowCompleted() {
		t.Fatal("engine run did not complete")
	}
	if werr := env.GetWorkflowError(); werr != nil {
		t.Fatalf("engine run error: %v", werr)
	}
	var result engine.RunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("engine run result: %v", err)
	}
	if result.Status != engine.StatusCompleted {
		t.Fatalf("run status = %q, want %q", result.Status, engine.StatusCompleted)
	}

	// engine-project half: the same call cmd/goobers/engineproject.go makes,
	// against the new ProjectionQuerier adapter instead of a dialed
	// client.Client.
	runsDir := t.TempDir()
	q := temporaltest.ProjectionQuerier{Env: env}
	dir, err := engine.ProjectCompletedRunForGaggle(context.Background(), q, in.RunID, gaggle.Name, runsDir, nil)
	if err != nil {
		t.Fatalf("project completed run: %v", err)
	}
	if dir == "" {
		t.Fatal("projected run directory is empty")
	}

	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("open projected journal: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read projected journal events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("projected journal has no events")
	}
	if got := events[0].Type; got != journal.EventRunStarted {
		t.Fatalf("first event = %q, want %q", got, journal.EventRunStarted)
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished {
		t.Fatalf("last event = %q, want %q", last.Type, journal.EventRunFinished)
	}
	if journal.RunPhase(last.Status) != journal.PhaseCompleted {
		t.Fatalf("run.finished status = %q, want %q", last.Status, journal.PhaseCompleted)
	}

	// Re-projecting an already-projected run is the documented idempotent
	// no-op (matching `goobers engine-project`'s "0 = projected or already
	// present" contract), not a second write.
	dir2, err := engine.ProjectCompletedRunForGaggle(context.Background(), q, in.RunID, gaggle.Name, runsDir, nil)
	if err != nil {
		t.Fatalf("re-project completed run: %v", err)
	}
	if dir2 != dir {
		t.Fatalf("re-project dir = %q, want %q", dir2, dir)
	}
}

// fakeGoober stands in for the external Copilot agent harness (un-CI-able),
// implementing invoke.Goober directly since this test drives
// engine.Activities without the gooberruntime/telemetry layer.
type fakeGoober struct{}

func (fakeGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: "engine start/project round trip goober completed",
		Outputs: map[string]interface{}{"pullRequest": "https://github.com/acme/web/pull/1"},
	}, nil
}

func (fakeGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "looks good"}, nil
}

// tempWorkspaces is the e2e engine.WorkspaceProvisioner — the engine fails
// closed without one (#621), since the closed invocation schema requires the
// envelope's workspace field. Temp-dir backed, standing in for the worker
// host's worktree-backed implementation (#632) at the same cluster boundary.
type tempWorkspaces struct{ t *testing.T }

func (p tempWorkspaces) Provision(context.Context, engine.WorkspaceRequest) (engine.Workspace, error) {
	return tempWorkspace{dir: p.t.TempDir()}, nil
}

type tempWorkspace struct{ dir string }

func (w tempWorkspace) Path() string                 { return w.dir }
func (w tempWorkspace) Remove(context.Context) error { return nil }
