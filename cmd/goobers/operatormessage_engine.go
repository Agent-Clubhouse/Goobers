package main

import (
	"context"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/engineoperator"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func (s *daemonRunJournalService) operatorMessageEngineDriven(gaggle, runID string) (bool, error) {
	reader, err := journal.OpenRead(filepath.Join(s.layout.ForGaggle(gaggle).RunsDir(), runID))
	if err != nil {
		return false, err
	}
	identity, err := reader.Identity()
	return identity.Driver == journal.DriverEngine, err
}

func (s *daemonRunJournalService) operatorMessageRunTerminal(gaggle, runID string) (bool, error) {
	reader, err := journal.OpenRead(filepath.Join(s.layout.ForGaggle(gaggle).RunsDir(), runID))
	if err != nil {
		return false, err
	}
	phase, err := reader.Phase()
	return phase != journal.PhaseRunning, err
}

func (s *daemonRunJournalService) submitEngineOperatorMessage(ctx context.Context, gaggle, runID string, request apiv1.OperatorMessageRequest) (httpapi.OperatorMessageSubmissionResponse, error) {
	return s.operatorMessages.Submit(ctx, gaggle, runID, request, engineoperator.Hooks{Terminal: s.operatorMessageRunTerminal, SelectMode: s.selectOperatorMessageDeliveryMode, TargetLive: s.operatorMessageTargetAddressLive, DeliverAccepted: s.deliverAcceptedOperatorMessage, Outcome: operatorMessageOutcome})
}

// withEngineOperatorMessageServices shares the writer and Temporal connection
// already owned by the daemon, including the scheduled-run workflow ID mapping.
func withEngineOperatorMessageServices(service *daemonRunJournalService, writer *livejournal.Writer, client *daemonEngineClient, guards *engineRunGuards) {
	service.operatorMessages.Writer = writer
	if client == nil || client.client == nil {
		return
	}
	deliverer := engine.NewOperatorMessageDeliverer(client.client)
	if guards != nil {
		deliverer = deliverer.WithWorkflowIDResolver(guards.resolveWorkflowID)
	}
	service.operatorMessages.Deliverer = deliverer
}
