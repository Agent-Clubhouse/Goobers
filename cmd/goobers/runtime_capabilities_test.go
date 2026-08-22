package main

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
)

func TestRuntimeMutationCapabilityParity(t *testing.T) {
	uiActions, err := loadUISurfaceActions()
	if err != nil {
		t.Fatal(err)
	}
	registries := []apicontract.SurfaceRegistry{
		{Surface: apicontract.SurfaceCLI, Actions: cliSurfaceActions()},
		{Surface: apicontract.SurfaceAPI, Actions: httpapi.SurfaceActions()},
		{Surface: apicontract.SurfaceUI, Actions: uiActions},
	}
	if err := apicontract.ValidateRuntimeParity(apicontract.V1RuntimeCapabilities(), registries); err != nil {
		t.Fatal(err)
	}
}

func TestActualSurfaceActionsAreExplicitlyClassified(t *testing.T) {
	assertActionClass(t, cliSurfaceActions(), "init", apicontract.ActionConfigTime)
	assertActionClass(t, cliSurfaceActions(), "up", apicontract.ActionDaemonLifecycle)
	assertActionClass(t, cliSurfaceActions(), "service install", apicontract.ActionDaemonLifecycle)
	assertActionClass(t, cliSurfaceActions(), "service uninstall", apicontract.ActionDaemonLifecycle)
	assertActionClass(t, cliSurfaceActions(), "service status", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "dashboard", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "versions", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "run", apicontract.ActionWorkflowExecution)
	assertActionClass(t, cliSurfaceActions(), "run abort", apicontract.ActionMaintenance)
	assertActionClass(t, cliSurfaceActions(), "scaffold goober", apicontract.ActionConfigTime)
	assertActionClass(t, cliSurfaceActions(), "scaffold workflow", apicontract.ActionConfigTime)
	assertActionClass(t, cliSurfaceActions(), "workflow show", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "runs list", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "runs du", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "completion bash", apicontract.ActionConfigTime)
	assertActionClass(t, cliSurfaceActions(), "completion zsh", apicontract.ActionConfigTime)
	assertActionClass(t, cliSurfaceActions(), "completion fish", apicontract.ActionConfigTime)
	assertActionClass(t, cliSurfaceActions(), "telemetry stats", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "telemetry errors", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "telemetry prune-orphans", apicontract.ActionMaintenance)
	assertActionClass(t, cliSurfaceActions(), "journal redact", apicontract.ActionMaintenance)
	assertActionClass(t, cliSurfaceActions(), "claims list", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "claims release", apicontract.ActionMaintenance)
	assertActionClass(t, cliSurfaceActions(), "status", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "escalations", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, cliSurfaceActions(), "escalations show", apicontract.ActionReadOnlyNavigation)

	apiActions := httpapi.SurfaceActions()
	if len(apiActions) != len(apicontract.V1Routes()) {
		t.Fatalf("API actions = %d, want one for each of %d registered routes", len(apiActions), len(apicontract.V1Routes()))
	}
	// Every route is read-only except the tier-2 intervention mutations
	// (approve/override/rerun, HITL-7/#469), the maintenance actions (the
	// local-only run reveal, and HITL escalation resolution — operator
	// recovery of a terminal run, kept outside the parity contract like
	// `run abort`), and the write planes' workflow-execution routes
	// (claims + trigger ingestion, #3509 §7).
	runtimeMutationRoutes := map[apicontract.ActionID]bool{"approveStage": true, "overrideStage": true, "rerunStage": true}
	maintenanceRoutes := map[apicontract.ActionID]bool{"runReveal": true, "resolveEscalation": true}
	workflowExecutionRoutes := map[apicontract.ActionID]bool{
		"claimAcquire": true, "claimRenew": true, "claimRelease": true, "claimSettle": true,
		"triggerIngest": true, "journalEmit": true,
	}
	for _, action := range apiActions {
		if runtimeMutationRoutes[action.ID] {
			if action.Class != apicontract.ActionRuntimeMutation {
				t.Fatalf("API action %q class = %q, want runtime-mutation", action.ID, action.Class)
			}
			continue
		}
		if maintenanceRoutes[action.ID] {
			if action.Class != apicontract.ActionMaintenance {
				t.Fatalf("API action %q class = %q, want maintenance", action.ID, action.Class)
			}
			continue
		}
		if workflowExecutionRoutes[action.ID] {
			if action.Class != apicontract.ActionWorkflowExecution {
				t.Fatalf("API action %q class = %q, want workflow-execution", action.ID, action.Class)
			}
			continue
		}
		if action.Class != apicontract.ActionReadOnlyNavigation {
			t.Fatalf("API action %q class = %q, want read-only", action.ID, action.Class)
		}
	}

	uiActions, err := loadUISurfaceActions()
	if err != nil {
		t.Fatal(err)
	}
	assertActionClass(t, uiActions, "navigate", apicontract.ActionReadOnlyNavigation)
	assertActionClass(t, uiActions, "revealRun", apicontract.ActionMaintenance)
}

func TestRuntimeCommandRegistersTypedCapability(t *testing.T) {
	registration := runtimeCommand("approve", "approve", nil)
	if registration.action != (apicontract.SurfaceAction{
		ID:         "approve",
		Class:      apicontract.ActionRuntimeMutation,
		Capability: "approve",
	}) {
		t.Fatalf("runtime command action = %+v", registration.action)
	}
}

func loadUISurfaceActions() ([]apicontract.SurfaceAction, error) {
	path := filepath.Join("..", "..", "portal", "src", "api", "surfaceActions.json")
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var registry map[apicontract.ActionID]struct {
		Class      apicontract.ActionClass  `json:"class"`
		Capability apicontract.CapabilityID `json:"capability"`
	}
	if err := json.Unmarshal(content, &registry); err != nil {
		return nil, err
	}
	actions := make([]apicontract.SurfaceAction, 0, len(registry))
	for id, registration := range registry {
		actions = append(actions, apicontract.SurfaceAction{
			ID:         id,
			Class:      registration.Class,
			Capability: registration.Capability,
		})
	}
	slices.SortFunc(actions, func(a, b apicontract.SurfaceAction) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return actions, nil
}

func assertActionClass(
	t *testing.T,
	actions []apicontract.SurfaceAction,
	id apicontract.ActionID,
	want apicontract.ActionClass,
) {
	t.Helper()
	for _, action := range actions {
		if action.ID == id {
			if action.Class != want {
				t.Fatalf("action %q class = %q, want %q", id, action.Class, want)
			}
			return
		}
	}
	t.Fatalf("action %q is not registered", id)
}
