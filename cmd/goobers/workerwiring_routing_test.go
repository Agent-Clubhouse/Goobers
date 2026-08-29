package main

import (
	"context"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
)

// TestEngineDispatchRoutesQueueAndGooberIdentityThroughWorkerSeams is #2904's
// acceptance path, run hermetically end to end through two pieces of
// production code that were extracted from the Goobernetes spike separately
// (#2921 for per-stage task-queue placement, #2935 for the worker seams and
// InvocationEnvelope.Goober) but had never been proven together:
//
//  1. Stage placement: internal/engine's stageTaskQueue routes a task
//     declaring a platform RequiredCapability onto the platform-suffixed
//     queue, verified here from inside the dispatched activity via
//     activity.GetInfo(ctx).TaskQueue — the queue a real worker would have
//     had to be polling to pick the task up at all.
//  2. Goober identity: the InvocationEnvelope the worker seam
//     (cmd/goobers/workerwiring.go's workerGoober) receives on the wire
//     carries the dispatching task's goober name, so it resolves the SAME
//     named executor the local runner would build for that stage
//     (runner.Config.NewAgentic) — not a different one, and not a
//     "no goober name" failure.
//
// Both engine.Run (dispatch) and workerSeams.Agentic() (the worker's
// dispatch-side seam) are real, unmodified production code; only the
// Temporal transport is substituted for the SDK's in-process test
// environment, exactly as test/e2e/engine_start_project_test.go's #2903
// round trip does for engine-start/engine-project.
func TestEngineDispatchRoutesQueueAndGooberIdentityThroughWorkerSeams(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder",
				Goal: "implement the change", Next: "windows-package",
			},
			{
				Name: "windows-package", Type: apiv1.TaskAgentic, Goober: "packager",
				Goal: "package for windows", RequiredCapabilities: []string{"os=windows"},
			},
		},
	}
	in := engine.RunInput{
		RunID:        "route-run",
		Gaggle:       "web",
		WorkflowName: "route",
		Version:      1,
		Spec:         spec,
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	}

	shared, scrub := journal.DefaultScrubber()
	var mu sync.Mutex
	requestedGoobers := map[string]int{}
	dispatchQueues := map[string]string{}
	seams := &workerSeams{
		scrubber: scrub,
		shared:   shared,
		byGaggle: map[string]*gaggleSeams{
			"web": {
				cfg: runner.Config{
					NewAgentic: func(name string, _ runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Goober, error) {
						return &routingCaptureGoober{
							name: name,
							onInvoke: func(ctx context.Context) {
								mu.Lock()
								defer mu.Unlock()
								requestedGoobers[name]++
								dispatchQueues[name] = activity.GetInfo(ctx).TaskQueue
							},
						}, nil
					},
				},
				runsDir: t.TempDir(),
			},
		},
	}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&engine.Activities{Goober: seams.Agentic(), Workspaces: routingTempWorkspaces{t: t}})
	env.ExecuteWorkflow(engine.Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if !env.IsWorkflowCompleted() {
		t.Fatal("engine run did not complete")
	}

	mu.Lock()
	defer mu.Unlock()

	// Goober identity: the worker resolved an executor for each task's OWN
	// goober name — not the wrong one, not an empty one (workerGoober.executor
	// fails closed on env.Goober == "", which was the #2904 gap: engine's
	// buildInvocation never set it).
	if requestedGoobers["coder"] != 1 {
		t.Errorf("coder executor constructed %d times, want 1", requestedGoobers["coder"])
	}
	if requestedGoobers["packager"] != 1 {
		t.Errorf("packager executor constructed %d times, want 1", requestedGoobers["packager"])
	}

	// Stage placement: only the task declaring the platform capability routes
	// off the workflow's own queue, and it routes to that queue's "-windows"
	// suffix (internal/engine's stageTaskQueue) — a real windows worker would
	// have had to poll exactly this queue to pick the stage up.
	inherited, routed := dispatchQueues["coder"], dispatchQueues["packager"]
	if inherited == "" {
		t.Fatal("implement dispatch carries no task queue at all")
	}
	if want := inherited + "-windows"; routed != want {
		t.Errorf("windows-package dispatched to queue %q, want %q (the workflow's own queue, platform-suffixed)", routed, want)
	}
}

// routingCaptureGoober is a per-dispatch invoke.Goober fake: onInvoke runs
// inside the activity, so it can read the ACTUAL task queue and goober name
// the worker seam dispatched with — the same information a real Temporal
// worker poller would only know by successfully having picked the task up
// from the right queue.
type routingCaptureGoober struct {
	name     string
	onInvoke func(ctx context.Context)
}

func (g *routingCaptureGoober) Invoke(ctx context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.onInvoke(ctx)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (g *routingCaptureGoober) Review(ctx context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.onInvoke(ctx)
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

// routingTempWorkspaces provisions a disposable temp-dir workspace per
// attempt, mirroring test/e2e's tempWorkspaces — the engine's activities
// provision a workspace before every stage dispatches (Activities.
// provisionWorkspace), so a real provisioner has to be wired even though this
// test cares only about queue and goober routing.
type routingTempWorkspaces struct{ t *testing.T }

func (p routingTempWorkspaces) Provision(context.Context, engine.WorkspaceRequest) (engine.Workspace, error) {
	return routingTempWorkspace{dir: p.t.TempDir()}, nil
}

type routingTempWorkspace struct{ dir string }

func (w routingTempWorkspace) Path() string                 { return w.dir }
func (w routingTempWorkspace) Remove(context.Context) error { return nil }
