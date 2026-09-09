package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// daemonEngineCancelService extends decision 005 D2 to authenticated remote
// operators. It admits only retained, currently owned engine runs. Acceptance
// is a request: engine settlement remains the only terminal writer and slot
// releaser, exactly as on the local engine cancellation path.
type daemonEngineCancelService struct {
	layout      instance.Layout
	definitions *interventionDefinitionRegistry
	guards      *engineRunGuards
	liveness    *engine.WorkflowLiveness
	log         *journal.InstanceLog
}

func newDaemonEngineCancelService(layout instance.Layout, definitions *interventionDefinitionRegistry, client *daemonEngineClient, guards *engineRunGuards, log *journal.InstanceLog) *daemonEngineCancelService {
	service := &daemonEngineCancelService{layout: layout, definitions: definitions, guards: guards, log: log}
	if client != nil && client.Temporal() != nil {
		service.liveness = engine.NewWorkflowLiveness(client.Temporal(), client.Namespace())
	}
	return service
}

func (s *daemonEngineCancelService) cancel(ctx context.Context, input httpapi.CancelRunRequest) (httpapi.CancelRunResult, bool, error) {
	reader, identity, err := s.resolve(input)
	if err != nil {
		var refusal *httpapi.InterventionError
		if errors.As(err, &refusal) && refusal.Code == "run_not_found" {
			// No retained engine identity: preserve the local plane's
			// established 200/not_running disposition for unknown IDs.
			return httpapi.CancelRunResult{}, false, nil
		}
		return httpapi.CancelRunResult{}, true, err
	}
	if !identity.EngineDriven() {
		return httpapi.CancelRunResult{}, false, nil
	}
	phase, err := reader.Phase()
	if err != nil {
		return httpapi.CancelRunResult{}, true, fmt.Errorf("read engine run phase: %w", err)
	}
	if phase != journal.PhaseRunning {
		return httpapi.CancelRunResult{Code: httpapi.CancelCodeTerminal, Phase: string(phase)}, true, nil
	}
	guards := s.guards
	if s.liveness != nil {
		// Resolve only within the admitted identity's gaggle, not the boot-time
		// owned set, which may have changed through a definition reload.
		guards = guards.withWorkflowIDResolver(func(ctx context.Context, runID string) (string, error) {
			return s.liveness.ResolveWorkflowID(ctx, runID, map[string]struct{}{identity.Gaggle: {}})
		})
	}
	workflowID, err := guards.cancelResolved(ctx, identity.RunID)
	if err != nil {
		return httpapi.CancelRunResult{}, true, err
	}
	if s.log != nil {
		// Like the local CLI, failure to record this diagnostic does not undo an
		// accepted cancellation or claim that the run has reached a terminal.
		_ = s.log.Append(journal.Event{
			Type: journal.EventRunnerAnnotation, Gaggle: identity.Gaggle, Workflow: identity.Workflow, RunID: identity.RunID,
			Runner: map[string]any{
				"kind": journal.RunnerAnnotationRunRecovery, "reason": "cancellation requested by an operator",
				"action": journal.RecoveryActionEngineCancelRequested, "driver": string(journal.DriverEngine),
				"workflowId": workflowID, "actor": input.Actor,
			},
		})
	}
	return httpapi.CancelRunResult{Code: httpapi.CancelCodeRequested}, true, nil
}

func (s *daemonEngineCancelService) resolve(input httpapi.CancelRunRequest) (*journal.Reader, journal.RunIdentity, error) {
	if !apiv1.ValidRunID(input.RunID) {
		return nil, journal.RunIdentity{}, runLookupError(http.StatusBadRequest, "invalid_run_id", "run ID is invalid")
	}
	definitions := s.definitions.Snapshot()
	owned := ownedGaggleSet(definitions.machines)
	gaggles := make([]string, 0, len(owned))
	for gaggle := range owned {
		gaggles = append(gaggles, gaggle)
	}
	found, err := locateOwnedRun(s.layout, gaggles, input.RunID)
	if err != nil {
		return nil, journal.RunIdentity{}, err
	}
	reader, err := journal.OpenReadOnly(found.dir)
	if err != nil {
		return nil, journal.RunIdentity{}, fmt.Errorf("read cancellation run: %w", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return nil, journal.RunIdentity{}, fmt.Errorf("read cancellation identity: %w", err)
	}
	if err := validateCancelIdentity(input, identity, found, definitions); err != nil {
		return nil, journal.RunIdentity{}, err
	}
	return reader, identity, nil
}

func validateCancelIdentity(input httpapi.CancelRunRequest, identity journal.RunIdentity, found ownedRunLocation, definitions interventionDefinitionSet) error {
	if identity.RunID != input.RunID || (found.gaggle != "" && identity.Gaggle != found.gaggle) {
		return runLookupError(http.StatusConflict, "run_identity_mismatch", "run identity does not match its runtime scope")
	}
	if _, owned := definitions.machines[localscheduler.WorkflowIdentity{Gaggle: identity.Gaggle, Workflow: identity.Workflow}]; !owned {
		return runLookupError(http.StatusNotFound, "run_not_owned", "run is not owned by this daemon's current definitions")
	}
	if (strings.TrimSpace(input.Gaggle) != "" && input.Gaggle != identity.Gaggle) ||
		(strings.TrimSpace(input.Workflow) != "" && input.Workflow != identity.Workflow) {
		return runLookupError(http.StatusConflict, "run_identity_mismatch", "requested gaggle or workflow does not match the run identity")
	}
	return nil
}
