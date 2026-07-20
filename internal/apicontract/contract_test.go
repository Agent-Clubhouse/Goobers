package apicontract

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRoutesRejectsContractDrift(t *testing.T) {
	expected := []Route{
		{
			ID:          RouteHealth,
			Method:      http.MethodGet,
			Path:        HealthPath,
			ActionClass: ActionReadOnlyNavigation,
		},
		{
			ID:          RouteRuns,
			Method:      http.MethodGet,
			Path:        RunsPath,
			ActionClass: ActionReadOnlyNavigation,
		},
	}
	tests := []struct {
		name   string
		actual []Route
		want   string
	}{
		{
			name:   "missing route",
			actual: expected[:1],
			want:   "missing",
		},
		{
			name: "unexpected route",
			actual: append(
				append([]Route{}, expected...),
				Route{
					ID:          RouteRunDetail,
					Method:      http.MethodGet,
					Path:        RunDetailPath,
					ActionClass: ActionReadOnlyNavigation,
				},
			),
			want: "unexpected",
		},
		{
			name: "method mismatch",
			actual: []Route{
				{
					ID:          RouteHealth,
					Method:      http.MethodPost,
					Path:        HealthPath,
					ActionClass: ActionReadOnlyNavigation,
				},
				expected[1],
			},
			want: "method",
		},
		{
			name: "path mismatch",
			actual: []Route{
				{
					ID:          RouteHealth,
					Method:      http.MethodGet,
					Path:        V1Prefix + "/ready",
					ActionClass: ActionReadOnlyNavigation,
				},
				expected[1],
			},
			want: "path",
		},
		{
			name: "action mismatch",
			actual: []Route{
				{
					ID:          RouteHealth,
					Method:      http.MethodGet,
					Path:        HealthPath,
					ActionClass: ActionConfigTime,
				},
				expected[1],
			},
			want: "action class",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRoutes(expected, test.actual)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateRoutes() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateRuntimeParityRejectsIncompleteAndDuplicateCapabilities(t *testing.T) {
	capabilities := []Capability{{ID: "approve", Class: ActionRuntimeMutation}}
	complete := []SurfaceRegistry{
		{Surface: SurfaceCLI, Actions: []SurfaceAction{runtimeAction("approve", "approve")}},
		{Surface: SurfaceAPI, Actions: []SurfaceAction{runtimeAction("approve", "approve")}},
		{Surface: SurfaceUI, Actions: []SurfaceAction{runtimeAction("approve", "approve")}},
	}
	if err := ValidateRuntimeParity(capabilities, complete); err != nil {
		t.Fatalf("complete registry: %v", err)
	}

	tests := []struct {
		name         string
		capabilities []Capability
		registries   []SurfaceRegistry
		want         string
	}{
		{
			name:         "missing CLI registration",
			capabilities: capabilities,
			registries: []SurfaceRegistry{
				{Surface: SurfaceCLI},
				complete[1],
				complete[2],
			},
			want: "missing cli registration",
		},
		{
			name:         "missing API registration",
			capabilities: capabilities,
			registries: []SurfaceRegistry{
				complete[0],
				{Surface: SurfaceAPI},
				complete[2],
			},
			want: "missing api registration",
		},
		{
			name:         "missing UI registration",
			capabilities: capabilities,
			registries: []SurfaceRegistry{
				complete[0],
				complete[1],
				{Surface: SurfaceUI},
			},
			want: "missing ui registration",
		},
		{
			name: "duplicate capability ID",
			capabilities: []Capability{
				capabilities[0],
				capabilities[0],
			},
			registries: complete,
			want:       "duplicated",
		},
		{
			name:         "duplicate surface registration",
			capabilities: capabilities,
			registries: []SurfaceRegistry{
				{
					Surface: SurfaceCLI,
					Actions: []SurfaceAction{
						runtimeAction("approve", "approve"),
						runtimeAction("approve-confirmation", "approve"),
					},
				},
				complete[1],
				complete[2],
			},
			want: "duplicate cli registration",
		},
		{
			name:         "duplicate action ID",
			capabilities: capabilities,
			registries: []SurfaceRegistry{
				{
					Surface: SurfaceCLI,
					Actions: []SurfaceAction{
						runtimeAction("approve", "approve"),
						{ID: "approve", Class: ActionReadOnlyNavigation},
					},
				},
				complete[1],
				complete[2],
			},
			want: "action ID",
		},
		{
			name:         "unknown registered capability",
			capabilities: capabilities,
			registries: []SurfaceRegistry{
				{
					Surface: SurfaceCLI,
					Actions: []SurfaceAction{runtimeAction("override", "override")},
				},
				complete[1],
				complete[2],
			},
			want: "unknown capability",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRuntimeParity(test.capabilities, test.registries)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateRuntimeParity() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestRuntimeParityExplicitlyExcludesNonMutationActions(t *testing.T) {
	for _, class := range []ActionClass{
		ActionReadOnlyNavigation,
		ActionConfigTime,
		ActionDaemonLifecycle,
		ActionWorkflowExecution,
		ActionMaintenance,
	} {
		t.Run(string(class), func(t *testing.T) {
			if RequiresRuntimeParity(class) {
				t.Fatalf("RequiresRuntimeParity(%q) = true", class)
			}
			registries := emptySurfaceRegistries()
			registries[0].Actions = []SurfaceAction{{ID: ActionID(class), Class: class}}
			if err := ValidateRuntimeParity(nil, registries); err != nil {
				t.Fatalf("excluded class requires a surface registration: %v", err)
			}
		})
	}
}

func TestValidateRuntimeParityRejectsMisclassifiedSurfaceActions(t *testing.T) {
	tests := []struct {
		name   string
		action SurfaceAction
		want   string
	}{
		{
			name:   "mutation without capability",
			action: SurfaceAction{ID: "approve", Class: ActionRuntimeMutation},
			want:   "has no capability",
		},
		{
			name: "excluded action with capability",
			action: SurfaceAction{
				ID:         "status",
				Class:      ActionReadOnlyNavigation,
				Capability: "approve",
			},
			want: "cannot register capability",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registries := emptySurfaceRegistries()
			registries[0].Actions = []SurfaceAction{test.action}
			err := ValidateRuntimeParity(nil, registries)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateRuntimeParity() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestV1ZeroMutationRegistryIsValid(t *testing.T) {
	capabilities := V1RuntimeCapabilities()
	if err := ValidateRuntimeParity(capabilities, emptySurfaceRegistries()); err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 0 {
		t.Fatalf("V1 registry = %+v, want zero runtime mutations", capabilities)
	}
}

func emptySurfaceRegistries() []SurfaceRegistry {
	return []SurfaceRegistry{
		{Surface: SurfaceCLI},
		{Surface: SurfaceAPI},
		{Surface: SurfaceUI},
	}
}

func runtimeAction(id ActionID, capability CapabilityID) SurfaceAction {
	return SurfaceAction{
		ID:         id,
		Class:      ActionRuntimeMutation,
		Capability: capability,
	}
}

func TestGeneratedPortalContractIsCurrent(t *testing.T) {
	want, err := TypeScriptContract()
	if err != nil {
		t.Fatal(err)
	}
	assertGeneratedFileCurrent(t, "contract.generated.ts", want)
}

func TestGeneratedPortalWireFixturesAreCurrent(t *testing.T) {
	want, err := TypeScriptWireFixtures()
	if err != nil {
		t.Fatal(err)
	}
	assertGeneratedFileCurrent(t, "wire.generated.ts", want)
}

func assertGeneratedFileCurrent(t *testing.T, name string, want []byte) {
	t.Helper()
	path := filepath.Join("..", "..", "portal", "src", "api", name)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("portal API contract %s is stale; run go generate ./internal/apicontract", name)
	}
}
