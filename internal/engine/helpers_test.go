package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

func boolPointer(value bool) *bool {
	return &value
}

// fakeWorkspaces is a WorkspaceProvisioner backed by temp directories. It
// records every request and teardown so tests can assert the fresh/disposable
// per-attempt workspace contract without git.
type fakeWorkspaces struct {
	mu       sync.Mutex
	root     string
	requests []WorkspaceRequest
	removed  []string
	// provisionErrs are consumed FIFO: each Provision call pops and returns
	// one until the script is exhausted, then provisioning succeeds.
	provisionErrs []error
	emptyPath     bool
	// publish, when set, is what a provisioned workspace reports from
	// PublishDelta (keyed by stage) — the fake's stand-in for the worker's
	// bundle-and-Put. Nil publishes nothing, the pre-#3803 self-arm shape.
	publish func(stage string) (WorkspaceDeltaPublication, error)
}

func testWorkspaces(t *testing.T) *fakeWorkspaces {
	t.Helper()
	return &fakeWorkspaces{root: t.TempDir()}
}

func (f *fakeWorkspaces) Provision(_ context.Context, req WorkspaceRequest) (Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.provisionErrs) > 0 {
		err := f.provisionErrs[0]
		f.provisionErrs = f.provisionErrs[1:]
		return nil, err
	}
	f.requests = append(f.requests, req)
	if f.emptyPath {
		return &fakeWorkspace{owner: f, stage: req.Stage}, nil
	}
	path, err := os.MkdirTemp(f.root, fmt.Sprintf("%s-%s-*", req.RunID, req.Stage))
	if err != nil {
		return nil, err
	}
	return &fakeWorkspace{owner: f, path: path, stage: req.Stage}, nil
}

func (f *fakeWorkspaces) provisioned() []WorkspaceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]WorkspaceRequest(nil), f.requests...)
}

func (f *fakeWorkspaces) removedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

type fakeWorkspace struct {
	owner *fakeWorkspaces
	path  string
	stage string
}

func (w *fakeWorkspace) Path() string { return w.path }

// PublishDelta implements DeltaPublisher through the owner's publish hook.
func (w *fakeWorkspace) PublishDelta(context.Context) (WorkspaceDeltaPublication, error) {
	w.owner.mu.Lock()
	publish := w.owner.publish
	w.owner.mu.Unlock()
	if publish == nil {
		return WorkspaceDeltaPublication{}, nil
	}
	return publish(w.stage)
}

func (w *fakeWorkspace) Remove(context.Context) error {
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	w.owner.removed = append(w.owner.removed, w.path)
	return os.RemoveAll(w.path)
}

// linearSpec is a single-stage, implement-only workflow — the shape the engine's
// happy-path run tests walk.
func linearSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement"},
		},
	}
}

// gatedSpec is an implement→review workflow whose reviewer gate can pass, abort,
// or loop back for changes — the shape the engine's branching tests walk.
func gatedSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          wf.TerminalComplete,
					"fail":          wf.TargetAbort,
					"needs-changes": "implement",
				},
			},
		},
	}
}
