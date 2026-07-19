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
	assertActionClass(t, cliSurfaceActions(), "status", apicontract.ActionReadOnlyNavigation)

	apiActions := httpapi.SurfaceActions()
	if len(apiActions) != len(apicontract.V1Routes()) {
		t.Fatalf("API actions = %d, want one for each of %d registered routes", len(apiActions), len(apicontract.V1Routes()))
	}
	for _, action := range apiActions {
		if action.Class != apicontract.ActionReadOnlyNavigation {
			t.Fatalf("API action %q class = %q, want read-only", action.ID, action.Class)
		}
	}

	uiActions, err := loadUISurfaceActions()
	if err != nil {
		t.Fatal(err)
	}
	assertActionClass(t, uiActions, "navigate", apicontract.ActionReadOnlyNavigation)
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
