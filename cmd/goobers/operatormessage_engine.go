package main

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type operatorMessageReceiptDeliverer interface {
	Deliver(context.Context, engine.OperatorMessageReceipt) (engine.OperatorMessageReceipt, error)
}

type engineOperatorMessageServices struct {
	writer    *livejournal.Writer
	deliverer operatorMessageReceiptDeliverer
	// Serialize backend delivery and its acknowledgement. Journal dedup handles
	// process restarts; this lock prevents concurrent retries driving one adapter
	// twice before the first call has durably acknowledged its delivery.
	mu sync.Mutex
}

func (s *daemonRunJournalService) operatorMessageEngineDriven(gaggle, runID string) (bool, error) {
	reader, err := journal.OpenRead(filepath.Join(s.layout.ForGaggle(gaggle).RunsDir(), runID))
	if err != nil {
		return false, err
	}
	identity, err := reader.Identity()
	return identity.Driver == journal.DriverEngine, err
}

func (s *daemonRunJournalService) submitEngineOperatorMessage(ctx context.Context, gaggle, runID string, request apiv1.OperatorMessageRequest) (httpapi.OperatorMessageSubmissionResponse, error) {
	s.operatorMessages.mu.Lock()
	defer s.operatorMessages.mu.Unlock()
	if s.operatorMessages.writer == nil {
		return httpapi.OperatorMessageSubmissionResponse{}, httpapi.NewInterventionError(http.StatusServiceUnavailable,
			"operator_messages_unavailable", "engine operator messages require the shared journal writer", nil)
	}
	terminal, err := s.operatorMessageRunTerminal(gaggle, runID)
	if err != nil {
		return httpapi.OperatorMessageSubmissionResponse{}, err
	}
	if !terminal && s.operatorMessages.deliverer == nil {
		return httpapi.OperatorMessageSubmissionResponse{}, httpapi.NewInterventionError(http.StatusServiceUnavailable,
			"operator_messages_unavailable", "engine operator messages require a Temporal receipt client", nil)
	}
	request.DeliveryMode = s.selectOperatorMessageDeliveryMode(gaggle, runID, request.TargetAddress, request.DeliveryMode)
	backend := s.operatorMessages.writer.OperatorMessages(gaggle, runID)
	record, accepted, err := backend.AcceptOperatorMessage(request)
	if err != nil {
		return httpapi.OperatorMessageSubmissionResponse{}, err
	}
	if record.Outcome == nil {
		record, err = s.finishEngineOperatorMessage(ctx, gaggle, runID, record, terminal)
		if err != nil {
			return httpapi.OperatorMessageSubmissionResponse{}, err
		}
	}
	response := httpapi.OperatorMessageSubmissionResponse{Accepted: accepted, Record: record}
	// Closed workflows cannot accept updates. Their journal still durably records
	// the typed rejection; never restart a workflow merely to record a receipt.
	if terminal {
		return response, nil
	}
	receipt := engine.OperatorMessageReceipt{
		RunID: runID, Reference: engine.OperatorMessageReference(runID, record.Request.IdempotencyKey),
		State: record.State, Code: record.Outcome.Code,
	}
	_, err = s.operatorMessages.deliverer.Deliver(ctx, receipt)
	return response, err
}

func (s *daemonRunJournalService) operatorMessageRunTerminal(gaggle, runID string) (bool, error) {
	reader, err := journal.OpenRead(filepath.Join(s.layout.ForGaggle(gaggle).RunsDir(), runID))
	if err != nil {
		return false, err
	}
	phase, err := reader.Phase()
	return phase != journal.PhaseRunning, err
}

func (s *daemonRunJournalService) finishEngineOperatorMessage(ctx context.Context, gaggle, runID string, record apiv1.OperatorMessageRecord, terminal bool) (apiv1.OperatorMessageRecord, error) {
	backend := s.operatorMessages.writer.OperatorMessages(gaggle, runID)
	request := record.Request
	if record.Acknowledgement != nil {
		return backend.CompleteOperatorMessage(operatorMessageOutcome(request, apiv1.OperatorMessageDelivered, "", ""))
	}
	if terminal {
		return backend.CompleteOperatorMessage(operatorMessageOutcome(request, apiv1.OperatorMessageRejected,
			"target_terminal", "target run is terminal"))
	}
	if !s.operatorMessageTargetAddressLive(gaggle, runID, request.TargetAddress) {
		return backend.CompleteOperatorMessage(operatorMessageOutcome(request, apiv1.OperatorMessageRejected,
			"target_unavailable", "target agent address is not live"))
	}
	if request.DeliveryMode == invoke.OperatorMessageModeBetweenTurn || request.DeliveryMode == invoke.OperatorMessageModeInterruptAndContinue {
		return s.deliverAcceptedOperatorMessage(ctx, gaggle, runID, record)
	}
	// Engine worker/pod adapters have no addressed live-message transport yet.
	// Do not claim next-attempt delivery: the engine has no queued-message consumer.
	return backend.CompleteOperatorMessage(operatorMessageOutcome(request, apiv1.OperatorMessageRejected,
		"live_delivery_unsupported", "target has no reachable live operator-message delivery channel"))
}

// withEngineOperatorMessageServices shares the writer and Temporal connection
// already owned by the daemon, including the scheduled-run workflow ID mapping.
func withEngineOperatorMessageServices(service *daemonRunJournalService, writer *livejournal.Writer, client *daemonEngineClient, guards *engineRunGuards) {
	service.operatorMessages.writer = writer
	if client == nil || client.client == nil {
		return
	}
	deliverer := engine.NewOperatorMessageDeliverer(client.client)
	if guards != nil {
		deliverer = deliverer.WithWorkflowIDResolver(guards.resolveWorkflowID)
	}
	service.operatorMessages.deliverer = deliverer
}
