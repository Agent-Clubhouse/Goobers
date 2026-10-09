package engine

import (
	"reflect"
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
)

type recordingWorker struct {
	workflowOpts []workflow.RegisterOptions
	workflows    []any
}

func (w *recordingWorker) RegisterWorkflow(interface{}) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterWorkflowWithOptions(fn interface{}, opts workflow.RegisterOptions) {
	w.workflowOpts = append(w.workflowOpts, opts)
	w.workflows = append(w.workflows, fn)
}

func (w *recordingWorker) RegisterDynamicWorkflow(interface{}, workflow.DynamicRegisterOptions) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterActivity(interface{}) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterActivityWithOptions(interface{}, activity.RegisterOptions) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterDynamicActivity(interface{}, activity.DynamicRegisterOptions) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterNexusService(*nexus.Service) {
	// Unused in this test.
}

func (w *recordingWorker) Start() error { return nil }

func (w *recordingWorker) Stop() {}

func (w *recordingWorker) Run(<-chan interface{}) error { return nil }

func TestRegisterWithPinsWorkflowsToCurrentBuild(t *testing.T) {
	w := &recordingWorker{}
	RegisterWith(w, &Activities{})

	want := []any{Run, DispatchOne, ChildDispatchOne}
	if got := len(w.workflowOpts); got != len(want) {
		t.Fatalf("registered workflows = %d, want %d", got, len(want))
	}
	for i, opts := range w.workflowOpts {
		if reflect.ValueOf(w.workflows[i]).Pointer() != reflect.ValueOf(want[i]).Pointer() {
			t.Fatalf("workflow %d has wrong implementation", i)
		}
		if got := opts.VersioningBehavior; got != workflow.VersioningBehaviorPinned {
			t.Fatalf("workflow %d versioning behavior = %v, want %v", i, got, workflow.VersioningBehaviorPinned)
		}
	}
}
