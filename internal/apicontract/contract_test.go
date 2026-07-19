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
		{ID: RouteHealth, Method: http.MethodGet, Path: HealthPath},
		{ID: RouteRuns, Method: http.MethodGet, Path: RunsPath},
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
				Route{ID: RouteRunDetail, Method: http.MethodGet, Path: RunDetailPath},
			),
			want: "unexpected",
		},
		{
			name: "method mismatch",
			actual: []Route{
				{ID: RouteHealth, Method: http.MethodPost, Path: HealthPath},
				expected[1],
			},
			want: "method",
		},
		{
			name: "path mismatch",
			actual: []Route{
				{ID: RouteHealth, Method: http.MethodGet, Path: V1Prefix + "/ready"},
				expected[1],
			},
			want: "path",
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
		{Surface: SurfaceCLI, Capabilities: []CapabilityID{"approve"}},
		{Surface: SurfaceAPI, Capabilities: []CapabilityID{"approve"}},
		{Surface: SurfaceUI, Capabilities: []CapabilityID{"approve"}},
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
				{Surface: SurfaceCLI, Capabilities: []CapabilityID{"approve", "approve"}},
				complete[1],
				complete[2],
			},
			want: "duplicate cli registration",
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
	} {
		t.Run(string(class), func(t *testing.T) {
			if RequiresRuntimeParity(class) {
				t.Fatalf("RequiresRuntimeParity(%q) = true", class)
			}
			capabilities := []Capability{{ID: CapabilityID(class), Class: class}}
			if err := ValidateRuntimeParity(capabilities, emptySurfaceRegistries()); err != nil {
				t.Fatalf("excluded class requires a surface registration: %v", err)
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

func TestGeneratedPortalContractIsCurrent(t *testing.T) {
	want, err := TypeScriptContract()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "portal", "src", "api", "contract.generated.ts")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("portal API contract is stale; run go generate ./internal/apicontract")
	}
}
