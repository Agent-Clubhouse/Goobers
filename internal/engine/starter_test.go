package engine

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// fakeRun is a minimal client.WorkflowRun for tests.
type fakeRun struct {
	id    string
	runID string
}

func (f fakeRun) GetID() string                          { return f.id }
func (f fakeRun) GetRunID() string                       { return f.runID }
func (f fakeRun) Get(context.Context, interface{}) error { return nil }
func (f fakeRun) GetWithOptions(context.Context, interface{}, client.WorkflowRunGetOptions) error {
	return nil
}

// fakeStarter is a fake workflowStarter capturing the last options.
type fakeStarter struct {
	run     client.WorkflowRun
	err     error
	gotOpts client.StartWorkflowOptions
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.gotOpts = opts
	return f.run, f.err
}

func sampleInput() RunInput {
	return RunInput{
		RunID:                  "web/flow/item-1",
		Gaggle:                 "web",
		WorkflowName:           "flow",
		Version:                1,
		PreviewFeaturesEnabled: true,
		Spec:                   linearSpec(),
	}
}

func TestRunIDDerivesDeterministicTraceID(t *testing.T) {
	if got := RunID("web", "flow", "item-1"); got != "367a0f0c2c9c47b4d4946044615a1c2f" {
		t.Errorf("RunID = %q, want deterministic trace id", got)
	}
	if got := RunID("web", "", "tick"); got != "05d327988d22595720a7870f6e7f2f73" {
		t.Errorf("RunID skipping empties = %q, want deterministic trace id", got)
	}
	if got := RunID(); got != "" {
		t.Errorf("RunID with no parts = %q, want empty", got)
	}
}

func TestTemporalStarterStartsRun(t *testing.T) {
	fs := &fakeStarter{run: fakeRun{id: "web/flow/item-1", runID: "exec-1"}}
	s := &TemporalStarter{client: fs, taskQueue: "goobers"}

	res, err := s.Start(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.AlreadyRunning {
		t.Error("expected a fresh start, got AlreadyRunning")
	}
	if res.RunID != "web/flow/item-1" {
		t.Errorf("RunID = %q, want web/flow/item-1", res.RunID)
	}
	// The starter must pin the workflow id and ask Temporal to error on duplicate.
	if fs.gotOpts.ID != "web/flow/item-1" {
		t.Errorf("opts.ID = %q, want the deterministic RunID", fs.gotOpts.ID)
	}
	if !fs.gotOpts.WorkflowExecutionErrorWhenAlreadyStarted {
		t.Error("expected WorkflowExecutionErrorWhenAlreadyStarted = true")
	}
	if fs.gotOpts.TaskQueue != "goobers" {
		t.Errorf("task queue = %q, want goobers", fs.gotOpts.TaskQueue)
	}
}

func TestTemporalStarterAlreadyRunningIsNoOp(t *testing.T) {
	fs := &fakeStarter{err: serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "exec-1")}
	s := &TemporalStarter{client: fs, taskQueue: "goobers"}

	res, err := s.Start(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("an already-started run must not be an error, got: %v", err)
	}
	if !res.AlreadyRunning {
		t.Error("expected AlreadyRunning = true for a duplicate start")
	}
	if res.RunID != "web/flow/item-1" {
		t.Errorf("RunID = %q, want the requested id", res.RunID)
	}
}

func TestTemporalStarterPropagatesOtherErrors(t *testing.T) {
	fs := &fakeStarter{err: errors.New("temporal unavailable")}
	s := &TemporalStarter{client: fs, taskQueue: "goobers"}
	if _, err := s.Start(context.Background(), sampleInput()); err == nil {
		t.Error("expected a non-already-started error to propagate")
	}
}

func TestTemporalStarterRequiresRunID(t *testing.T) {
	s := &TemporalStarter{client: &fakeStarter{}, taskQueue: "goobers"}
	in := sampleInput()
	in.RunID = ""
	if _, err := s.Start(context.Background(), in); err == nil {
		t.Error("expected an error when RunID is empty")
	}
}
