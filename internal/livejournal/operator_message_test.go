package livejournal

import (
	"context"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestOperatorMessagesShareActiveWriterAndSurviveIdleClose(t *testing.T) {
	w, dir := testWriter(t)
	const runID = "operator-shared-writer"
	if _, err := w.Emit(context.Background(), openBatch(runID, time.Now())); err != nil {
		t.Fatal(err)
	}
	backend := w.OperatorMessages("web", runID)
	request := apiv1.OperatorMessageRequest{
		Schema: apiv1.OperatorMessageRequestSchema, RequestID: "request", IdempotencyKey: "key",
		TargetAddress: "terminal:operator", PrincipalRef: "operator", RequestedAt: time.Now(), Purpose: "review",
		Content: apiv1.OperatorMessageContent{Text: "please inspect"}, DeliveryMode: "terminal",
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := backend.AcceptOperatorMessage(request); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	records := journal.ReplayOperatorMessages(readEvents(t, dir, runID))
	if len(records) != 1 {
		t.Fatalf("durable records = %d, want 1", len(records))
	}
	w.CloseIdle(-time.Second)
	outcome := apiv1.OperatorMessageOutcome{
		Schema: apiv1.OperatorMessageOutcomeSchema, RequestID: request.RequestID, IdempotencyKey: request.IdempotencyKey,
		CompletedAt: time.Now(), Status: apiv1.OperatorMessageRejected, Code: "live_delivery_unsupported",
	}
	record, err := backend.CompleteOperatorMessage(outcome)
	if err != nil || record.Outcome == nil {
		t.Fatalf("after idle close = %+v, %v", record, err)
	}
	// A resumed emitter must reacquire cleanly; the bound backend released its
	// transient handle and reservation after completing the message.
	if _, err := w.Emit(context.Background(), EmitRequest{RunID: runID, Gaggle: "web", Ops: []Op{
		appendOp("after-message", time.Now(), journal.Event{Type: journal.EventRunnerAnnotation, Reason: "test"}),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.OperatorMessages("other", runID).AcceptOperatorMessage(request); err == nil {
		t.Fatal("accepted foreign gaggle")
	}
}
