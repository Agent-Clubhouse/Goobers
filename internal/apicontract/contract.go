// Package apicontract defines the versioned daemon routes and runtime mutation
// capabilities shared by the Go API and portal client.
package apicontract

//go:generate go run ./cmd/generate -contract-output ../../portal/src/api/contract.generated.ts -fixtures-output ../../portal/src/api/wire.generated.ts

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Versioned V1 route paths.
const (
	// V1Prefix is the versioned root for daemon API routes.
	V1Prefix = "/api/v1"

	HealthPath                   = V1Prefix + "/health"
	InstancePath                 = V1Prefix + "/instance"
	PortalConfigPath             = V1Prefix + "/portal/config"
	GagglesPath                  = V1Prefix + "/gaggles"
	GaggleGoobersPath            = V1Prefix + "/gaggles/{gaggle}/goobers"
	GaggleWorkflowsPath          = V1Prefix + "/gaggles/{gaggle}/workflows"
	GaggleConnectionsPath        = V1Prefix + "/gaggles/{gaggle}/connections"
	WorkflowDetailPath           = V1Prefix + "/gaggles/{gaggle}/workflows/{workflow}"
	RunsPath                     = V1Prefix + "/runs"
	RunDetailPath                = V1Prefix + "/runs/{run}"
	RunRevealPath                = V1Prefix + "/runs/{run}/reveal"
	RunEventsPath                = V1Prefix + "/runs/{run}/events"
	StageAttemptsPath            = V1Prefix + "/runs/{run}/stages/{stage}/attempts"
	RunArtifactPath              = V1Prefix + "/runs/{run}/artifacts/{digest}"
	RunTranscriptPath            = V1Prefix + "/runs/{run}/transcripts/{seq}"
	TelemetryStatsPath           = V1Prefix + "/telemetry/stats"
	TelemetryErrorSignaturesPath = V1Prefix + "/telemetry/error-signatures"
	TelemetryErrorsPath          = V1Prefix + "/telemetry/errors"
	EventsPath                   = V1Prefix + "/events"

	// Tier-2 human-intervention mutation routes. The CLI and dashboard use this
	// same API-first surface, behind the shared access-control seam.
	RunStageApprovePath  = V1Prefix + "/runs/{run}/stages/{stage}/approve"
	RunStageOverridePath = V1Prefix + "/runs/{run}/stages/{stage}/override"
	RunStageRerunPath    = V1Prefix + "/runs/{run}/stages/{stage}/rerun"
)

// RouteID is the stable cross-adapter identity of a versioned route.
type RouteID string

// Stable V1 route IDs.
const (
	RouteHealth                   RouteID = "health"
	RouteInstance                 RouteID = "instance"
	RoutePortalConfig             RouteID = "portalConfig"
	RouteGaggles                  RouteID = "gaggles"
	RouteGaggleGoobers            RouteID = "gaggleGoobers"
	RouteGaggleWorkflows          RouteID = "gaggleWorkflows"
	RouteGaggleConnections        RouteID = "gaggleConnections"
	RouteWorkflowDetail           RouteID = "workflowDetail"
	RouteRuns                     RouteID = "runs"
	RouteRunDetail                RouteID = "runDetail"
	RouteRunReveal                RouteID = "runReveal"
	RouteRunEvents                RouteID = "runEvents"
	RouteStageAttempts            RouteID = "stageAttempts"
	RouteRunArtifact              RouteID = "runArtifact"
	RouteRunTranscript            RouteID = "runTranscript"
	RouteTelemetryStats           RouteID = "telemetryStats"
	RouteTelemetryErrorSignatures RouteID = "telemetryErrorSignatures"
	RouteTelemetryErrors          RouteID = "telemetryErrors"
	RouteEvents                   RouteID = "events"

	RouteApproveStage  RouteID = "approveStage"
	RouteOverrideStage RouteID = "overrideStage"
	RouteRerunStage    RouteID = "rerunStage"
)

// Route is one method and path in the versioned daemon contract.
type Route struct {
	ID          RouteID
	Method      string
	Path        string
	ActionClass ActionClass
	Capability  CapabilityID
	// Cost is what this route costs to serve, as a class rather than a guess
	// (#1926, §7.1). Required on every route: a contract test rejects a route
	// with no class, which is how "no path is unclassified" becomes enforced
	// rather than documented.
	Cost CostClass
	// Budget bounds how long the server will spend on this route. Required and
	// non-zero, except for Stream routes where a deadline would cut the stream.
	//
	// Every budget stays strictly below the client's 10s abort, so a request the
	// server gives up on is reported as a 503 the client can act on rather than
	// racing the client's own timeout.
	Budget time.Duration
}

// CostClass names what a route costs to serve.
//
// The classes are about the SHAPE of the work, not its measured duration —
// duration is a consequence. A class tells you what the route touches, which is
// what decides whether it can be pooled alongside another one.
type CostClass string

const (
	// CostBounded is a supported filter combination answered from the read
	// model: indexed, page-limited, and zero journal opens (§5.7).
	CostBounded CostClass = "bounded"
	// CostSingleRun reads one run's journal, once per fingerprint. Bounded by a
	// single run's size rather than by history.
	CostSingleRun CostClass = "single-run"
	// CostAggregate is answered from pre-aggregated buckets rather than by
	// scanning the rows behind them (§6.4).
	CostAggregate CostClass = "aggregate"
	// CostBlob streams stored bytes — an artifact or a transcript. Separated
	// from single-run because its budget is dominated by transfer size, not by
	// query time, which is why it carries a much larger one.
	CostBlob CostClass = "blob"
	// CostStream is a long-lived subscription. The ONLY class permitted a zero
	// budget: a deadline on an SSE response cuts the stream mid-flight, which is
	// indistinguishable to a client from the server dying.
	CostStream CostClass = "stream"
	// CostMutation is a runtime write.
	CostMutation CostClass = "mutation"
)

// Route budgets.
//
// These come from Wave 0's measured p99.9 against §14.12's absolute targets, not
// from taste, and every one is strictly below the portal's 10s client abort.
const (
	// BoundedBudget covers indexed list and aggregate reads. Measured p50 for a
	// read-model list page is single-digit milliseconds; 8s is three orders of
	// magnitude of headroom, and is a backstop against pathology rather than a
	// target.
	BoundedBudget = 8 * time.Second
	// BlobBudget covers artifact and transcript streaming, where the time is
	// transfer rather than query. A large artifact over a slow link legitimately
	// takes longer than any query should.
	BlobBudget = 60 * time.Second
	// MutationBudget covers approve/override/rerun. Kept at the bounded budget:
	// a mutation that cannot be accepted in 8s is not going to be accepted.
	MutationBudget = 8 * time.Second
)

var v1Routes = []Route{
	{ID: RouteHealth, Method: http.MethodGet, Path: HealthPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBounded, Budget: BoundedBudget},
	{ID: RouteInstance, Method: http.MethodGet, Path: InstancePath, ActionClass: ActionReadOnlyNavigation, Cost: CostAggregate, Budget: BoundedBudget},
	{ID: RoutePortalConfig, Method: http.MethodGet, Path: PortalConfigPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBounded, Budget: BoundedBudget},
	{ID: RouteGaggles, Method: http.MethodGet, Path: GagglesPath, ActionClass: ActionReadOnlyNavigation, Cost: CostAggregate, Budget: BoundedBudget},
	{ID: RouteGaggleGoobers, Method: http.MethodGet, Path: GaggleGoobersPath, ActionClass: ActionReadOnlyNavigation, Cost: CostAggregate, Budget: BoundedBudget},
	{ID: RouteGaggleWorkflows, Method: http.MethodGet, Path: GaggleWorkflowsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostAggregate, Budget: BoundedBudget},
	{ID: RouteGaggleConnections, Method: http.MethodGet, Path: GaggleConnectionsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostAggregate, Budget: BoundedBudget},
	{ID: RouteWorkflowDetail, Method: http.MethodGet, Path: WorkflowDetailPath, ActionClass: ActionReadOnlyNavigation, Cost: CostAggregate, Budget: BoundedBudget},
	{ID: RouteRuns, Method: http.MethodGet, Path: RunsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBounded, Budget: BoundedBudget},
	{ID: RouteRunDetail, Method: http.MethodGet, Path: RunDetailPath, ActionClass: ActionReadOnlyNavigation, Cost: CostSingleRun, Budget: BoundedBudget},
	{ID: RouteRunReveal, Method: http.MethodPost, Path: RunRevealPath, ActionClass: ActionMaintenance, Cost: CostMutation, Budget: MutationBudget},
	{ID: RouteRunEvents, Method: http.MethodGet, Path: RunEventsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostSingleRun, Budget: BoundedBudget},
	{ID: RouteStageAttempts, Method: http.MethodGet, Path: StageAttemptsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostSingleRun, Budget: BoundedBudget},
	{ID: RouteRunArtifact, Method: http.MethodGet, Path: RunArtifactPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBlob, Budget: BlobBudget},
	{ID: RouteRunTranscript, Method: http.MethodGet, Path: RunTranscriptPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBlob, Budget: BlobBudget},
	{ID: RouteTelemetryStats, Method: http.MethodGet, Path: TelemetryStatsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostAggregate, Budget: BoundedBudget},
	{ID: RouteTelemetryErrorSignatures, Method: http.MethodGet, Path: TelemetryErrorSignaturesPath, ActionClass: ActionReadOnlyNavigation, Cost: CostAggregate, Budget: BoundedBudget},
	{ID: RouteTelemetryErrors, Method: http.MethodGet, Path: TelemetryErrorsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostAggregate, Budget: BoundedBudget},
	{ID: RouteEvents, Method: http.MethodGet, Path: EventsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostStream, Budget: 0},

	{ID: RouteApproveStage, Method: http.MethodPost, Path: RunStageApprovePath, ActionClass: ActionRuntimeMutation, Capability: "approve", Cost: CostMutation, Budget: MutationBudget},
	{ID: RouteOverrideStage, Method: http.MethodPost, Path: RunStageOverridePath, ActionClass: ActionRuntimeMutation, Capability: "override", Cost: CostMutation, Budget: MutationBudget},
	{ID: RouteRerunStage, Method: http.MethodPost, Path: RunStageRerunPath, ActionClass: ActionRuntimeMutation, Capability: "rerun", Cost: CostMutation, Budget: MutationBudget},
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
		if got.ActionClass != want.ActionClass {
			return fmt.Errorf("route %q action class is %q, want %q", id, got.ActionClass, want.ActionClass)
		}
		if got.Capability != want.Capability {
			return fmt.Errorf("route %q capability is %q, want %q", id, got.Capability, want.Capability)
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
		if route.ID == "" || route.Method == "" || route.Path == "" || route.ActionClass == "" {
			return nil, fmt.Errorf("%s route has an empty ID, method, path, or action class", name)
		}
		action := route.SurfaceAction()
		if err := validateActionShape(action); err != nil {
			return nil, fmt.Errorf("%s route %q: %w", name, route.ID, err)
		}
		switch route.ActionClass {
		case ActionReadOnlyNavigation:
			if route.Method == http.MethodGet || route.Method == http.MethodHead {
				break
			}
			return nil, fmt.Errorf(
				"%s route %q uses method %q for a read-only action",
				name,
				route.ID,
				route.Method,
			)
		case ActionRuntimeMutation:
			if route.Method != http.MethodGet && route.Method != http.MethodHead {
				break
			}
			return nil, fmt.Errorf(
				"%s route %q uses method %q for a runtime mutation",
				name,
				route.ID,
				route.Method,
			)
		case ActionMaintenance:
			if route.Method != http.MethodGet && route.Method != http.MethodHead {
				break
			}
			return nil, fmt.Errorf(
				"%s route %q uses method %q for a maintenance action",
				name,
				route.ID,
				route.Method,
			)
		default:
			return nil, fmt.Errorf(
				"%s route %q action class %q is not valid for an API route",
				name,
				route.ID,
				route.ActionClass,
			)
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
// parity. Every surface registration is classified explicitly rather than
// inferred from command or route names.
type ActionClass string

// Action classes participating in or explicitly excluded from runtime parity.
const (
	// ActionRuntimeMutation is an operator intervention that must be available
	// through every product surface.
	ActionRuntimeMutation    ActionClass = "runtime-mutation"
	ActionReadOnlyNavigation ActionClass = "read-only-navigation"
	ActionConfigTime         ActionClass = "config-time"
	ActionDaemonLifecycle    ActionClass = "daemon-lifecycle"
	// ActionWorkflowExecution starts or advances the workflow machinery; it is
	// not an operator intervention in an existing run.
	ActionWorkflowExecution ActionClass = "workflow-execution"
	// ActionMaintenance repairs local journals or instance budgets outside the
	// cross-surface runtime capability contract.
	ActionMaintenance ActionClass = "maintenance"
)

// ActionID is a stable identity within one registered product surface.
type ActionID string

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

// SurfaceAction attaches parity classification to an actual adapter
// registration. Capability is required only for runtime mutations.
type SurfaceAction struct {
	ID         ActionID     `json:"id"`
	Class      ActionClass  `json:"class"`
	Capability CapabilityID `json:"capability,omitempty"`
}

// SurfaceAction returns the action registered by this route.
func (r Route) SurfaceAction() SurfaceAction {
	return SurfaceAction{
		ID:         ActionID(r.ID),
		Class:      r.ActionClass,
		Capability: r.Capability,
	}
}

// SurfaceRegistry is the action registration owned by one adapter.
type SurfaceRegistry struct {
	Surface Surface
	Actions []SurfaceAction
}

// v1RuntimeCapabilities lists V1's first runtime mutations (HITL-7/#469):
// approve, override, and rerun. Every registered CLI/API/UI surface action
// referencing one of these is a deliberate stub today (the real gate
// resolution and rerun-wiring land separately, #466/#468) — this registry,
// and ValidateRuntimeParity's enforcement of it, exist so every surface
// stays in lockstep as those land, rather than drifting until a parity gap
// is discovered late.
var v1RuntimeCapabilities = []Capability{
	{ID: "approve", Class: ActionRuntimeMutation},
	{ID: "override", Class: ActionRuntimeMutation},
	{ID: "rerun", Class: ActionRuntimeMutation},
}

// V1RuntimeCapabilities returns V1's registered runtime mutation
// capabilities (HITL-7/#469): approve, override, and rerun.
func V1RuntimeCapabilities() []Capability {
	return slices.Clone(v1RuntimeCapabilities)
}

// RequiresRuntimeParity reports whether an action class must be registered on
// CLI, API, and UI surfaces.
func RequiresRuntimeParity(class ActionClass) bool {
	return class == ActionRuntimeMutation
}

// ValidateRuntimeParity enforces unique capability IDs and complete CLI/API/UI
// registration for every runtime mutation. Registrations contain the actions
// actually dispatched by each surface, not a separate capability list.
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
		surfaceCapabilities := make(map[CapabilityID]struct{}, len(registry.Actions))
		actionIDs := make(map[ActionID]struct{}, len(registry.Actions))
		for _, action := range registry.Actions {
			if err := validateActionShape(action); err != nil {
				return fmt.Errorf("%s action: %w", registry.Surface, err)
			}
			if _, exists := actionIDs[action.ID]; exists {
				return fmt.Errorf("%s action ID %q is duplicated", registry.Surface, action.ID)
			}
			actionIDs[action.ID] = struct{}{}
			if !RequiresRuntimeParity(action.Class) {
				continue
			}
			capability, ok := capabilities[action.Capability]
			if !ok {
				return fmt.Errorf(
					"%s action %q references unknown capability %q",
					registry.Surface,
					action.ID,
					action.Capability,
				)
			}
			if !RequiresRuntimeParity(capability.Class) {
				return fmt.Errorf(
					"excluded capability %q cannot have a runtime registration",
					action.Capability,
				)
			}
			if _, exists := surfaceCapabilities[action.Capability]; exists {
				return fmt.Errorf(
					"capability %q has duplicate %s registration",
					action.Capability,
					registry.Surface,
				)
			}
			surfaceCapabilities[action.Capability] = struct{}{}
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

func validateActionShape(action SurfaceAction) error {
	if action.ID == "" {
		return fmt.Errorf("action ID is empty")
	}
	if !validActionClass(action.Class) {
		return fmt.Errorf("action %q has unknown class %q", action.ID, action.Class)
	}
	if RequiresRuntimeParity(action.Class) {
		if action.Capability == "" {
			return fmt.Errorf("runtime mutation action %q has no capability", action.ID)
		}
		return nil
	}
	if action.Capability != "" {
		return fmt.Errorf(
			"excluded action %q cannot register capability %q",
			action.ID,
			action.Capability,
		)
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
	case ActionRuntimeMutation,
		ActionReadOnlyNavigation,
		ActionConfigTime,
		ActionDaemonLifecycle,
		ActionWorkflowExecution,
		ActionMaintenance:
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
		output.WriteString(", actionClass: ")
		output.WriteString(strconv.Quote(string(route.ActionClass)))
		if route.Capability != "" {
			output.WriteString(", capability: ")
			output.WriteString(strconv.Quote(string(route.Capability)))
		}
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
	output.WriteString("\nexport const actionClasses = {\n")
	output.WriteString("  runtimeMutation: \"runtime-mutation\",\n")
	output.WriteString("  readOnlyNavigation: \"read-only-navigation\",\n")
	output.WriteString("  configTime: \"config-time\",\n")
	output.WriteString("  daemonLifecycle: \"daemon-lifecycle\",\n")
	output.WriteString("  workflowExecution: \"workflow-execution\",\n")
	output.WriteString("  maintenance: \"maintenance\",\n")
	output.WriteString("} as const;\n\n")
	output.WriteString("export type ActionClass =\n")
	output.WriteString("  (typeof actionClasses)[keyof typeof actionClasses];\n\n")
	output.WriteString("export type SurfaceAction =\n")
	output.WriteString("  | { id: string; class: typeof actionClasses.runtimeMutation; capability: RuntimeMutationCapabilityId }\n")
	output.WriteString("  | { id: string; class: Exclude<ActionClass, typeof actionClasses.runtimeMutation>; capability?: never };\n")
	return []byte(output.String()), nil
}
