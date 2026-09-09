package engine

import (
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
)

type recordingWorker struct {
	workflowOpts []workflow.RegisterOptions
}

func (w *recordingWorker) RegisterWorkflow(interface{}) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterWorkflowWithOptions(_ interface{}, opts workflow.RegisterOptions) {
	w.workflowOpts = append(w.workflowOpts, opts)
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

	if got := len(w.workflowOpts); got != 5 {
		t.Fatalf("registered workflows = %d, want 5", got)
	}
	for i, opts := range w.workflowOpts {
		if got := opts.VersioningBehavior; got != workflow.VersioningBehaviorPinned {
			t.Fatalf("workflow %d versioning behavior = %v, want %v", i, got, workflow.VersioningBehaviorPinned)
		}
	}
}
