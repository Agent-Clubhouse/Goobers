package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestOperatorMessageUpdateHistoryReplaysWithoutCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	const queue = "operator-message-receipts"
	const runID = "operator-message-history"
	hitlReplayWorker(t, c, queue, hitlFailingExec())
	in := hitlInput(t)
	in.RunID = runID
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: runID, TaskQueue: queue}, Run, in)
	if err != nil {
		t.Fatal(err)
	}
	const bearer = "fixture-bearer-credential"
	const content = "fixture-secret-message-content"
	receipt := OperatorMessageReceipt{
		RunID: runID, Reference: OperatorMessageReference(runID, bearer+content),
		State: apiv1.OperatorMessageState(apiv1.OperatorMessageRejected), Code: "live_delivery_unsupported",
	}
	deliverer := NewOperatorMessageDeliverer(c)
	for range 2 {
		got, err := deliverer.Deliver(ctx, receipt)
		if err != nil || got != receipt {
			t.Fatalf("Deliver = %+v, %v", got, err)
		}
	}
	// A distinct update ID exercises workflow-level replay dedup too, rather
	// than relying exclusively on the server's update-ID deduplication.
	handle, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		UpdateID: "second-transport-attempt", WorkflowID: runID, UpdateName: OperatorMessageUpdateName,
		Args: []interface{}{receipt}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	var duplicate OperatorMessageReceipt
	if err := handle.Get(ctx, &duplicate); err != nil || duplicate != receipt {
		t.Fatalf("duplicate = %+v, %v", duplicate, err)
	}
	conflicting := receipt
	conflicting.Code = "target_terminal"
	conflictHandle, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		UpdateID: "conflicting-attempt", WorkflowID: runID, UpdateName: OperatorMessageUpdateName,
		Args: []interface{}{conflicting}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err == nil {
		err = conflictHandle.Get(ctx, &duplicate)
	}
	if err == nil {
		t.Fatal("accepted conflicting outcome for one durable reference")
	}
	if err := c.CancelWorkflow(ctx, runID, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.Get(ctx, nil); err == nil {
		t.Fatal("cancelled workflow returned no error")
	}
	history := hitlFetchHistory(t, ctx, c, runID, run.GetRunID())
	// Proto String renders payload bytes as text; JSON would base64-encode
	// them and could hide a leaked credential from this assertion.
	raw := history.String()
	if strings.Contains(raw, bearer) || strings.Contains(raw, content) {
		t.Fatal("history contains credential material")
	}
	hitlReplay(t, history)
}

func TestOperatorMessageReceiptRejectsFreeTextAndWrongRun(t *testing.T) {
	receipt := OperatorMessageReceipt{RunID: "run-1", Reference: OperatorMessageReference("run-1", "key"),
		State: apiv1.OperatorMessageState(apiv1.OperatorMessageRejected), Code: "live_delivery_unsupported"}
	if err := receipt.validate("run-2"); err == nil {
		t.Fatal("accepted wrong run")
	}
	receipt.Code = "secret-bearing-arbitrary-text"
	if err := receipt.validate("run-1"); err == nil {
		t.Fatal("accepted free-form code")
	}
	receipt.Code = ""
	receipt.State = apiv1.OperatorMessageAccepted
	if err := receipt.validate("run-1"); err == nil {
		t.Fatal("claimed queued delivery without consumer")
	}
}

type operatorReceiptUpdateClient struct{ addressed []string }

func (c *operatorReceiptUpdateClient) UpdateWorkflow(_ context.Context, opts client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error) {
	c.addressed = append(c.addressed, opts.WorkflowID)
	if opts.WorkflowID != "scheduled-workflow" {
		return nil, serviceerror.NewNotFound("not found")
	}
	return &operatorReceiptHandle{hitlFakeHandle: hitlFakeHandle{workflowID: opts.WorkflowID}, receipt: opts.Args[0].(OperatorMessageReceipt)}, nil
}

type operatorReceiptHandle struct {
	hitlFakeHandle
	receipt OperatorMessageReceipt
}

func (h operatorReceiptHandle) Get(_ context.Context, out interface{}) error {
	*out.(*OperatorMessageReceipt) = h.receipt
	return nil
}
func TestOperatorMessageDelivererResolvesScheduledRun(t *testing.T) {
	fake := &operatorReceiptUpdateClient{}
	deliverer := (&OperatorMessageDeliverer{client: fake}).WithWorkflowIDResolver(func(context.Context, string) (string, error) {
		return "scheduled-workflow", nil
	})
	receipt := OperatorMessageReceipt{RunID: "run-1", Reference: OperatorMessageReference("run-1", "key"),
		State: apiv1.OperatorMessageState(apiv1.OperatorMessageRejected), Code: "live_delivery_unsupported"}
	got, err := deliverer.Deliver(context.Background(), receipt)
	if err != nil || got != receipt {
		t.Fatalf("Deliver = %+v, %v", got, err)
	}
	if len(fake.addressed) != 2 || fake.addressed[0] != "run-1" || fake.addressed[1] != "scheduled-workflow" {
		t.Fatalf("addressed = %v", fake.addressed)
	}
}
