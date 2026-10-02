package engineoperator

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type receiptSink struct{}

func (receiptSink) Deliver(_ context.Context, r engine.OperatorMessageReceipt) (engine.OperatorMessageReceipt, error) {
	return r, nil
}

func fixture(t *testing.T) (*Service, Hooks, apiv1.OperatorMessageRequest) {
	t.Helper()
	root := t.TempDir()
	run, err := journal.Create(root, journal.RunIdentity{RunID: "engine-message-run", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "web", Driver: journal.DriverEngine, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return root, gaggle == "web" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	service := &Service{Writer: writer, Deliverer: receiptSink{}}
	hooks := Hooks{Terminal: func(string, string) (bool, error) { return false, nil }, SelectMode: func(_, _, _, mode string) string { return mode }, TargetLive: func(string, string, string) bool { t.Fatal("acknowledged request consulted live target"); return false }, Outcome: func(request apiv1.OperatorMessageRequest, status apiv1.OperatorMessageOutcomeStatus, code, detail string) apiv1.OperatorMessageOutcome {
		return apiv1.OperatorMessageOutcome{Schema: apiv1.OperatorMessageOutcomeSchema, RequestID: request.RequestID, IdempotencyKey: request.IdempotencyKey, CompletedAt: time.Now().UTC(), Status: status, Code: code, Detail: detail}
	}}
	request := apiv1.OperatorMessageRequest{Schema: apiv1.OperatorMessageRequestSchema, RequestID: "request", IdempotencyKey: "acknowledged", TargetAddress: "terminal:operator", PrincipalRef: "operator", RequestedAt: time.Now(), Purpose: "review", Content: apiv1.OperatorMessageContent{Text: "please inspect"}, DeliveryMode: invoke.OperatorMessageModeBetweenTurn}
	return service, hooks, request
}
func TestEngineOperatorMessageRecoversAcknowledgedDelivery(t *testing.T) {
	service, hooks, request := fixture(t)
	backend := service.Writer.OperatorMessages("web", "engine-message-run")
	if _, _, err := backend.AcceptOperatorMessage(request); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.AcknowledgeOperatorMessage(apiv1.OperatorMessageAcknowledgement{Schema: apiv1.OperatorMessageAcknowledgementSchema, RequestID: request.RequestID, IdempotencyKey: request.IdempotencyKey, PrincipalRef: "operator", AcknowledgedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// Simulate restart after adapter acknowledgement, before outcome append.
	// No target is registered now, but the durable acknowledgement still wins.
	response, err := service.Submit(context.Background(), "web", "engine-message-run", request, hooks)
	if err != nil || response.Record.Outcome == nil || response.Record.Outcome.Status != apiv1.OperatorMessageDelivered {
		t.Fatalf("acknowledged recovery = %+v, %v", response, err)
	}
}

func TestTerminalAndUnavailableTargetsKeepDurableTypedOutcomes(t *testing.T) {
	for _, test := range []struct {
		name           string
		terminal, live bool
		mode, code     string
	}{{"terminal", true, false, "terminal", "target_terminal"}, {"unavailable", false, false, "terminal", "target_unavailable"}, {"unsupported", false, true, "terminal", "live_delivery_unsupported"}} {
		t.Run(test.name, func(t *testing.T) {
			service, hooks, request := fixture(t)
			hooks.Terminal = func(string, string) (bool, error) { return test.terminal, nil }
			hooks.TargetLive = func(string, string, string) bool { return test.live }
			request.DeliveryMode = test.mode
			for i := range 2 {
				response, err := service.Submit(t.Context(), "web", "engine-message-run", request, hooks)
				if err != nil || response.Accepted != (i == 0) || response.Record.Outcome == nil || response.Record.Outcome.Code != test.code {
					t.Fatalf("outcome: %+v %v", response, err)
				}
			}
		})
	}
	service, hooks, request := fixture(t)
	sentinel := errors.New("terminal read failure")
	hooks.Terminal = func(string, string) (bool, error) { return false, sentinel }
	if _, err := service.Submit(t.Context(), "web", "engine-message-run", request, hooks); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	service.Writer = nil
	if _, err := service.Submit(t.Context(), "web", "engine-message-run", request, hooks); err == nil {
		t.Fatal("missing writer accepted")
	}
}
