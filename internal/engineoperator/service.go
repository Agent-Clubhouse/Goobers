// Package engineoperator orchestrates durable engine operator-message delivery.
package engineoperator

import (
	"context"
	"net/http"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/livejournal"
)

// ReceiptDeliverer submits content-free durable outcomes to the engine.
type ReceiptDeliverer interface {
	Deliver(context.Context, engine.OperatorMessageReceipt) (engine.OperatorMessageReceipt, error)
}

// Service serializes delivery and durable acknowledgement for a daemon lifetime.
type Service struct {
	Writer    *livejournal.Writer
	Deliverer ReceiptDeliverer
	// Serialize backend delivery and its acknowledgement. Journal dedup handles
	// process restarts; this lock prevents concurrent retries driving one adapter
	// twice before the first call has durably acknowledged its delivery.
	mu sync.Mutex
}

// Hooks retain daemon-owned mode, target, and live-adapter resolution.
type Hooks struct {
	Terminal        func(string, string) (bool, error)
	SelectMode      func(string, string, string, string) string
	TargetLive      func(string, string, string) bool
	DeliverAccepted func(context.Context, string, string, apiv1.OperatorMessageRecord) (apiv1.OperatorMessageRecord, error)
	Outcome         func(apiv1.OperatorMessageRequest, apiv1.OperatorMessageOutcomeStatus, string, string) apiv1.OperatorMessageOutcome
}

// Submit preserves journal deduplication and backend receipt retry ordering.
func (s *Service) Submit(ctx context.Context, gaggle, runID string, request apiv1.OperatorMessageRequest, hooks Hooks) (httpapi.OperatorMessageSubmissionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Writer == nil {
		return httpapi.OperatorMessageSubmissionResponse{}, httpapi.NewInterventionError(http.StatusServiceUnavailable,
			"operator_messages_unavailable", "engine operator messages require the shared journal writer", nil)
	}
	terminal, err := hooks.Terminal(gaggle, runID)
	if err != nil {
		return httpapi.OperatorMessageSubmissionResponse{}, err
	}
	if !terminal && s.Deliverer == nil {
		return httpapi.OperatorMessageSubmissionResponse{}, httpapi.NewInterventionError(http.StatusServiceUnavailable,
			"operator_messages_unavailable", "engine operator messages require a Temporal receipt client", nil)
	}
	request.DeliveryMode = hooks.SelectMode(gaggle, runID, request.TargetAddress, request.DeliveryMode)
	backend := s.Writer.OperatorMessages(gaggle, runID)
	record, accepted, err := backend.AcceptOperatorMessage(request)
	if err != nil {
		return httpapi.OperatorMessageSubmissionResponse{}, err
	}
	if record.Outcome == nil {
		record, err = s.finish(ctx, gaggle, runID, record, terminal, hooks)
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
	_, err = s.Deliverer.Deliver(ctx, receipt)
	return response, err
}

func (s *Service) finish(ctx context.Context, gaggle, runID string, record apiv1.OperatorMessageRecord, terminal bool, hooks Hooks) (apiv1.OperatorMessageRecord, error) {
	backend := s.Writer.OperatorMessages(gaggle, runID)
	request := record.Request
	if record.Acknowledgement != nil {
		return backend.CompleteOperatorMessage(hooks.Outcome(request, apiv1.OperatorMessageDelivered, "", ""))
	}
	if terminal {
		return backend.CompleteOperatorMessage(hooks.Outcome(request, apiv1.OperatorMessageRejected,
			"target_terminal", "target run is terminal"))
	}
	if !hooks.TargetLive(gaggle, runID, request.TargetAddress) {
		return backend.CompleteOperatorMessage(hooks.Outcome(request, apiv1.OperatorMessageRejected,
			"target_unavailable", "target agent address is not live"))
	}
	if request.DeliveryMode == invoke.OperatorMessageModeBetweenTurn || request.DeliveryMode == invoke.OperatorMessageModeInterruptAndContinue {
		return hooks.DeliverAccepted(ctx, gaggle, runID, record)
	}
	// Engine worker/pod adapters have no addressed live-message transport yet.
	// Do not claim next-attempt delivery: the engine has no queued-message consumer.
	return backend.CompleteOperatorMessage(hooks.Outcome(request, apiv1.OperatorMessageRejected,
		"live_delivery_unsupported", "target has no reachable live operator-message delivery channel"))
}
