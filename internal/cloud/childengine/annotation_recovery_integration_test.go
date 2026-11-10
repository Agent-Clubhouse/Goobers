//go:build cloudintegration

package childengine

import (
	"context"
	"errors"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
)

// Legacy Temporal state is held fixed while real PG admission adopts its lost
// confirmation. The fake rejects every attempted new start, as Temporal does
// for the existing workflow ID; no workflow execution is simulated here.
type legacyAnnotationExecution struct {
	client.Client
	description *workflowservice.DescribeWorkflowExecutionResponse
}

func (c *legacyAnnotationExecution) ExecuteWorkflow(context.Context, client.StartWorkflowOptions, interface{}, ...interface{}) (client.WorkflowRun, error) {
	return nil, serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "request", "execution")
}

func (c *legacyAnnotationExecution) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return c.description, nil
}

type annotationRecoveryGeneration struct{ generation, gaggle, workflow string }

func (g annotationRecoveryGeneration) Admit(generation, gaggle, workflow string) error {
	if generation != g.generation || gaggle != g.gaggle || workflow != g.workflow {
		return errors.New("recovery selected a different generation or workflow")
	}
	return nil
}

func TestPGAnnotationRecoveryKeepsStartedInputDigest(t *testing.T) {
	f := newFamily(t)
	record, _, err := f.store.Accept(t.Context(), "manual/annotation-upgrade", "operator", []byte(`{"workflow":"flow"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	legacy := engine.RunInput{AcceptanceID: record.ID, Gaggle: "example", WorkflowName: "flow", Version: 1,
		WorkflowDigest: "sha256:annotated-workflow", GooberDigest: "sha256:goober", ConfigGeneration: "sha256:retained-generation"}
	// The old start made it to Temporal, but its confirmation never reached PG.
	legacy, err = acceptance.NewStartAdmission(f.store).AdmitStart(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := startfence.InputDigest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	owner := startfence.StartOwnerMemo{AcceptanceID: legacy.AcceptanceID, OwnerEpoch: legacy.OwnerEpoch,
		StarterID: legacy.StarterID, InstanceID: legacy.InstanceID, InputDigest: digest}
	payload, err := converter.GetDefaultDataConverter().ToPayload(owner)
	if err != nil {
		t.Fatal(err)
	}
	temporal := &legacyAnnotationExecution{description: &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Type: &commonpb.WorkflowType{Name: "Run"},
			Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
			Memo:   &commonpb.Memo{Fields: map[string]*commonpb.Payload{startfence.StartOwnerMemoKey: payload}}}}}
	if err := f.manager.Release(t.Context(), f.handle); err != nil {
		t.Fatal(err)
	}
	f.takeOver(t, "annotation-successor")
	controller, err := acceptance.New(controllerPool(t, f.pool), testInstance)
	if err != nil {
		t.Fatal(err)
	}
	controller.BindFence(f.manager, f.handle)
	starter := startfence.New(temporal, "cloud", acceptance.NewStartAdmission(controller),
		annotationRecoveryGeneration{legacy.ConfigGeneration, legacy.Gaggle, legacy.WorkflowName})
	resolved := legacy
	resolved.Annotations = map[string]string{"goobers.dev/allow-preview-features": "true"}
	for attempt := range 2 {
		result, err := starter.Start(t.Context(), resolved)
		if err != nil || !result.Adopt || result.NotMine {
			t.Fatalf("attempt %d: %+v, %v", attempt, result, err)
		}
		pins, err := controller.RunPinsOf(t.Context(), legacy.RunID)
		if err != nil {
			t.Fatal(err)
		}
		want, err := acceptance.RunPinsFor(legacy)
		if err != nil || pins != want {
			t.Fatalf("adoption rewrote started pins: %+v, want %+v: %v", pins, want, err)
		}
		if pending, err := controller.StartPending(t.Context(), legacy.AcceptanceID); err != nil || pending {
			t.Fatalf("legacy acceptance still dispatching: %v, %v", pending, err)
		}
		parents := ParentAuthority{Execution: func(ctx context.Context, runID string) (startfence.StartedExecution, error) {
			return startfence.DescribeStarted(ctx, temporal, runID)
		}}
		if err := parents.boundExecution(t.Context(), pins); err != nil {
			t.Fatalf("adopted pins no longer bind their execution: %v", err)
		}
	}
}
