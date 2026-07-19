// Package apicontract defines the versioned daemon routes and runtime mutation
// capabilities shared by the Go API and portal client.
package apicontract

//go:generate go run ./cmd/generate -output ../../portal/src/api/contract.generated.ts

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Versioned V1 route paths.
const (
	// V1Prefix is the versioned root for daemon API routes.
	V1Prefix = "/api/v1"

	HealthPath          = V1Prefix + "/health"
	InstancePath        = V1Prefix + "/instance"
	GagglesPath         = V1Prefix + "/gaggles"
	GaggleGoobersPath   = V1Prefix + "/gaggles/{gaggle}/goobers"
	GaggleWorkflowsPath = V1Prefix + "/gaggles/{gaggle}/workflows"
	WorkflowDetailPath  = V1Prefix + "/gaggles/{gaggle}/workflows/{workflow}"
	RunsPath            = V1Prefix + "/runs"
	RunDetailPath       = V1Prefix + "/runs/{run}"
	RunEventsPath       = V1Prefix + "/runs/{run}/events"
	StageAttemptsPath   = V1Prefix + "/runs/{run}/stages/{stage}/attempts"
	RunArtifactPath     = V1Prefix + "/runs/{run}/artifacts/{digest}"
	TelemetryStatsPath  = V1Prefix + "/telemetry/stats"
	TelemetryErrorsPath = V1Prefix + "/telemetry/errors"
	EventsPath          = V1Prefix + "/events"
)

// RouteID is the stable cross-adapter identity of a versioned route.
type RouteID string

// Stable V1 route IDs.
const (
	RouteHealth          RouteID = "health"
	RouteInstance        RouteID = "instance"
	RouteGaggles         RouteID = "gaggles"
	RouteGaggleGoobers   RouteID = "gaggleGoobers"
	RouteGaggleWorkflows RouteID = "gaggleWorkflows"
	RouteWorkflowDetail  RouteID = "workflowDetail"
	RouteRuns            RouteID = "runs"
	RouteRunDetail       RouteID = "runDetail"
	RouteRunEvents       RouteID = "runEvents"
	RouteStageAttempts   RouteID = "stageAttempts"
	RouteRunArtifact     RouteID = "runArtifact"
	RouteTelemetryStats  RouteID = "telemetryStats"
	RouteTelemetryErrors RouteID = "telemetryErrors"
	RouteEvents          RouteID = "events"
)

// Route is one method and path in the versioned daemon contract.
type Route struct {
	ID     RouteID
	Method string
	Path   string
}

var v1Routes = []Route{
	{ID: RouteHealth, Method: http.MethodGet, Path: HealthPath},
	{ID: RouteInstance, Method: http.MethodGet, Path: InstancePath},
	{ID: RouteGaggles, Method: http.MethodGet, Path: GagglesPath},
	{ID: RouteGaggleGoobers, Method: http.MethodGet, Path: GaggleGoobersPath},
	{ID: RouteGaggleWorkflows, Method: http.MethodGet, Path: GaggleWorkflowsPath},
	{ID: RouteWorkflowDetail, Method: http.MethodGet, Path: WorkflowDetailPath},
	{ID: RouteRuns, Method: http.MethodGet, Path: RunsPath},
	{ID: RouteRunDetail, Method: http.MethodGet, Path: RunDetailPath},
	{ID: RouteRunEvents, Method: http.MethodGet, Path: RunEventsPath},
	{ID: RouteStageAttempts, Method: http.MethodGet, Path: StageAttemptsPath},
	{ID: RouteRunArtifact, Method: http.MethodGet, Path: RunArtifactPath},
	{ID: RouteTelemetryStats, Method: http.MethodGet, Path: TelemetryStatsPath},
	{ID: RouteTelemetryErrors, Method: http.MethodGet, Path: TelemetryErrorsPath},
	{ID: RouteEvents, Method: http.MethodGet, Path: EventsPath},
}

// V1Routes returns an isolated copy of the versioned route contract.
func V1Routes() []Route {
	return slices.Clone(v1Routes)
}

// V1Route looks up a route by its stable ID.
func V1Route(id RouteID) (Route, bool) {
	for _, route := range v1Routes {
		if route.ID == id {
			return route, true
		}
	}
	return Route{}, false
}

// ValidateRoutes requires two route registries to match by ID, method, and path.
func ValidateRoutes(expected, actual []Route) error {
	expectedByID, err := indexRoutes("expected", expected)
	if err != nil {
		return err
	}
	actualByID, err := indexRoutes("actual", actual)
	if err != nil {
		return err
	}

	for _, id := range sortedRouteIDs(expectedByID) {
		want := expectedByID[id]
		got, ok := actualByID[id]
		if !ok {
			return fmt.Errorf("route %q is missing", id)
		}
		if got.Method != want.Method {
			return fmt.Errorf("route %q method is %q, want %q", id, got.Method, want.Method)
		}
		if got.Path != want.Path {
			return fmt.Errorf("route %q path is %q, want %q", id, got.Path, want.Path)
		}
	}
	for _, id := range sortedRouteIDs(actualByID) {
		if _, ok := expectedByID[id]; !ok {
			return fmt.Errorf("route %q is unexpected", id)
		}
	}
	return nil
}

func indexRoutes(name string, routes []Route) (map[RouteID]Route, error) {
	indexed := make(map[RouteID]Route, len(routes))
	for _, route := range routes {
		if route.ID == "" || route.Method == "" || route.Path == "" {
			return nil, fmt.Errorf("%s route has an empty ID, method, or path", name)
		}
		if _, exists := indexed[route.ID]; exists {
			return nil, fmt.Errorf("%s route ID %q is duplicated", name, route.ID)
		}
		indexed[route.ID] = route
	}
	return indexed, nil
}

func sortedRouteIDs(routes map[RouteID]Route) []RouteID {
	ids := make([]RouteID, 0, len(routes))
	for id := range routes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ActionClass determines whether an action participates in runtime mutation
// parity. Read-only, config-time, and daemon-lifecycle actions are explicit
// exclusions rather than inferred from command or route names.
type ActionClass string

// Action classes participating in or explicitly excluded from runtime parity.
const (
	ActionRuntimeMutation    ActionClass = "runtime-mutation"
	ActionReadOnlyNavigation ActionClass = "read-only-navigation"
	ActionConfigTime         ActionClass = "config-time"
	ActionDaemonLifecycle    ActionClass = "daemon-lifecycle"
)

// CapabilityID is the stable identity of a runtime capability.
type CapabilityID string

// Surface is an adapter that must expose each runtime mutation capability.
type Surface string

// Runtime mutation registration surfaces.
const (
	SurfaceCLI Surface = "cli"
	SurfaceAPI Surface = "api"
	SurfaceUI  Surface = "ui"
)

// Capability declares an action and its parity class.
type Capability struct {
	ID    CapabilityID
	Class ActionClass
}

// SurfaceRegistry is the runtime mutation registration owned by one adapter.
type SurfaceRegistry struct {
	Surface      Surface
	Capabilities []CapabilityID
}

var v1RuntimeCapabilities []Capability

// V1RuntimeCapabilities is intentionally empty: V1 has no runtime mutations,
// while retaining the typed registry needed by future approve, override, and
// rerun capabilities.
func V1RuntimeCapabilities() []Capability {
	return slices.Clone(v1RuntimeCapabilities)
}

// RequiresRuntimeParity reports whether an action class must be registered on
// CLI, API, and UI surfaces.
func RequiresRuntimeParity(class ActionClass) bool {
	return class == ActionRuntimeMutation
}

// ValidateRuntimeParity enforces unique capability IDs and complete,
// independently owned CLI/API/UI registration for every runtime mutation.
func ValidateRuntimeParity(capabilityList []Capability, registries []SurfaceRegistry) error {
	capabilities, err := indexCapabilities(capabilityList)
	if err != nil {
		return err
	}

	registered := make(map[Surface]map[CapabilityID]struct{}, 3)
	for _, registry := range registries {
		if !validSurface(registry.Surface) {
			return fmt.Errorf("runtime registry has unknown surface %q", registry.Surface)
		}
		if _, exists := registered[registry.Surface]; exists {
			return fmt.Errorf("%s runtime registry is duplicated", registry.Surface)
		}
		surfaceCapabilities := make(map[CapabilityID]struct{}, len(registry.Capabilities))
		for _, id := range registry.Capabilities {
			capability, ok := capabilities[id]
			if !ok {
				return fmt.Errorf("%s registration references unknown capability %q", registry.Surface, id)
			}
			if !RequiresRuntimeParity(capability.Class) {
				return fmt.Errorf("excluded capability %q cannot have a runtime registration", id)
			}
			if _, exists := surfaceCapabilities[id]; exists {
				return fmt.Errorf("capability %q has duplicate %s registration", id, registry.Surface)
			}
			surfaceCapabilities[id] = struct{}{}
		}
		registered[registry.Surface] = surfaceCapabilities
	}

	for _, surface := range []Surface{SurfaceCLI, SurfaceAPI, SurfaceUI} {
		if _, ok := registered[surface]; !ok {
			return fmt.Errorf("%s runtime registry is missing", surface)
		}
	}
	for _, capability := range capabilityList {
		if !RequiresRuntimeParity(capability.Class) {
			continue
		}
		for _, surface := range []Surface{SurfaceCLI, SurfaceAPI, SurfaceUI} {
			if _, ok := registered[surface][capability.ID]; !ok {
				return fmt.Errorf("capability %q is missing %s registration", capability.ID, surface)
			}
		}
	}
	return nil
}

func indexCapabilities(capabilityList []Capability) (map[CapabilityID]Capability, error) {
	capabilities := make(map[CapabilityID]Capability, len(capabilityList))
	for _, capability := range capabilityList {
		if capability.ID == "" {
			return nil, fmt.Errorf("capability ID is empty")
		}
		if !validActionClass(capability.Class) {
			return nil, fmt.Errorf("capability %q has unknown class %q", capability.ID, capability.Class)
		}
		if _, exists := capabilities[capability.ID]; exists {
			return nil, fmt.Errorf("capability ID %q is duplicated", capability.ID)
		}
		capabilities[capability.ID] = capability
	}
	return capabilities, nil
}

func validActionClass(class ActionClass) bool {
	switch class {
	case ActionRuntimeMutation, ActionReadOnlyNavigation, ActionConfigTime, ActionDaemonLifecycle:
		return true
	default:
		return false
	}
}

func validSurface(surface Surface) bool {
	switch surface {
	case SurfaceCLI, SurfaceAPI, SurfaceUI:
		return true
	default:
		return false
	}
}

// TypeScriptContract renders the checked-in contract consumed by the portal.
func TypeScriptContract() ([]byte, error) {
	if err := ValidateRoutes(v1Routes, v1Routes); err != nil {
		return nil, fmt.Errorf("validate route contract: %w", err)
	}
	runtimeCapabilities := V1RuntimeCapabilities()
	if _, err := indexCapabilities(runtimeCapabilities); err != nil {
		return nil, fmt.Errorf("validate runtime capability contract: %w", err)
	}

	var output strings.Builder
	output.WriteString("// Code generated by go generate ./internal/apicontract; DO NOT EDIT.\n\n")
	output.WriteString("export const apiRoutes = {\n")
	for _, route := range v1Routes {
		output.WriteString("  ")
		output.WriteString(strconv.Quote(string(route.ID)))
		output.WriteString(": { method: ")
		output.WriteString(strconv.Quote(route.Method))
		output.WriteString(", path: ")
		output.WriteString(strconv.Quote(route.Path))
		output.WriteString(" },\n")
	}
	output.WriteString("} as const;\n\n")
	output.WriteString("export type ApiRoute = (typeof apiRoutes)[keyof typeof apiRoutes];\n\n")
	output.WriteString("export const runtimeMutationCapabilities = [")
	first := true
	for _, capability := range runtimeCapabilities {
		if !RequiresRuntimeParity(capability.Class) {
			continue
		}
		if !first {
			output.WriteString(", ")
		}
		output.WriteString(strconv.Quote(string(capability.ID)))
		first = false
	}
	output.WriteString("] as const;\n\n")
	output.WriteString("export type RuntimeMutationCapabilityId =\n")
	output.WriteString("  (typeof runtimeMutationCapabilities)[number];\n")
	return []byte(output.String()), nil
}
