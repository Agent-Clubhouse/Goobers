package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type operatorReceiptSink struct {
	receipts []engine.OperatorMessageReceipt
	fail     bool
}

func (s *operatorReceiptSink) Deliver(_ context.Context, receipt engine.OperatorMessageReceipt) (engine.OperatorMessageReceipt, error) {
	s.receipts = append(s.receipts, receipt)
	if s.fail {
		s.fail = false
		return engine.OperatorMessageReceipt{}, errors.New("receipt transport unavailable")
	}
	return receipt, nil
}

func engineOperatorMessageFixture(t *testing.T) (*daemonRunJournalService, string, *operatorReceiptSink) {
	t.Helper()
	layout := crossRunTestLayout(t)
	const runID = "engine-message-run"
	run, err := journal.Create(layout.ForGaggle(crossRunTestGaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: crossRunTestGaggle,
		Driver: journal.DriverEngine, Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	seq := seedOperatorMessageLiveAgent(t, layout, crossRunTestGaggle, runID, "implement", 1, "agent")
	address := operatorMessageAgentAddress(t, runID, "implement", 1, "agent", seq)
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) {
		return layout.ForGaggle(gaggle).RunsDir(), gaggle == crossRunTestGaggle
	}, livejournal.WithScrubber(journal.NewPatternScrubber()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	event := journal.Event{Type: journal.EventRunnerAnnotation, Reason: "fixture"}
	_, err = writer.Emit(context.Background(), livejournal.EmitRequest{
		RunID: runID, Gaggle: crossRunTestGaggle,
		Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "fixture", Time: time.Now(), Event: &event}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &operatorReceiptSink{}
	service := newDaemonRunJournalService(layout, nil)
	service.operatorMessages.Writer = writer
	service.operatorMessages.Deliverer = sink
	return service, address, sink
}

func engineMessageRequest(address, key string) httpapi.OperatorMessageSubmissionRequest {
	request := operatorMessageRequest("engine-message-run", key, httpapi.Principal{Subject: "operator", Roles: []httpapi.Role{httpapi.RoleOperate}})
	request.TargetAddress = address
	return request
}

func TestEngineOperatorMessageDurableUnsupportedAndDuplicate(t *testing.T) {
	service, address, sink := engineOperatorMessageFixture(t)
	secret := "ghp_" + strings.Repeat("e", 36)
	request := engineMessageRequest(address, "key-"+secret)
	request.Content.Text = "Authorization: Bearer " + secret
	for index := range 2 {
		response, err := service.SubmitOperatorMessage(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if response.Accepted != (index == 0) || response.Record.Outcome == nil || response.Record.Outcome.Code != "live_delivery_unsupported" {
			t.Fatalf("response = %+v", response)
		}
	}
	reader, err := journal.OpenRead(filepath.Join(service.layout.ForGaggle(crossRunTestGaggle).RunsDir(), request.RunID))
	if err != nil {
		t.Fatal(err)
	}
	records, err := reader.OperatorMessages()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	raw, err := json.Marshal(sink.receipts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "Authorization") {
		t.Fatal("Temporal receipt contains content or credentials")
	}
	if len(sink.receipts) != 2 || sink.receipts[0] != sink.receipts[1] {
		t.Fatalf("receipts = %+v", sink.receipts)
	}
}

func TestEngineOperatorMessageReachableAdapterDeliveredOnceAcrossReceiptRetry(t *testing.T) {
	service, address, sink := engineOperatorMessageFixture(t)
	target := registerOperatorMessageTarget(t, address, []string{invoke.OperatorMessageModeBetweenTurn}, nil)
	sink.fail = true
	request := engineMessageRequest(address, "retry")
	if _, err := service.SubmitOperatorMessage(context.Background(), request); err == nil {
		t.Fatal("receipt failure was hidden")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := service.SubmitOperatorMessage(context.Background(), request)
			if err != nil || response.Record.State != apiv1.OperatorMessageState(apiv1.OperatorMessageDelivered) {
				t.Errorf("response = %+v, %v", response, err)
			}
		}()
	}
	wg.Wait()
	if len(target.deliveries) != 1 {
		t.Fatalf("adapter deliveries = %d, want 1", len(target.deliveries))
	}
}

func TestEngineOperatorMessageAuthorizationAndTerminalTargets(t *testing.T) {
	service, address, sink := engineOperatorMessageFixture(t)
	denied := engineMessageRequest(address, "denied")
	denied.Principal.Roles = []httpapi.Role{httpapi.RoleView}
	response, err := service.SubmitOperatorMessage(context.Background(), denied)
	if !isInterventionStatus(err, http.StatusForbidden) || response.Record.Outcome == nil || len(sink.receipts) != 0 {
		t.Fatalf("denial = %+v, %v; receipts %d", response, err, len(sink.receipts))
	}
	event := journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}
	_, err = service.operatorMessages.Writer.Emit(context.Background(), livejournal.EmitRequest{
		RunID: "engine-message-run", Gaggle: crossRunTestGaggle,
		Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "finished", Time: time.Now(), Event: &event}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err = service.SubmitOperatorMessage(context.Background(), engineMessageRequest(address, "terminal"))
	if err != nil || response.Record.Outcome == nil || response.Record.Outcome.Code != "target_terminal" || len(sink.receipts) != 0 {
		t.Fatalf("terminal = %+v, %v; receipts %d", response, err, len(sink.receipts))
	}
}
