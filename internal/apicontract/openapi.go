package apicontract

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

var pathParameterPattern = regexp.MustCompile(`\{([^}]+)\}`)

// OpenAPIDocument renders the daemon's machine-readable HTTP contract,
// including any optional routes configured on this daemon.
func OpenAPIDocument(authenticated bool, optionalRoutes ...Route) ([]byte, error) {
	paths := map[string]any{}
	routes := append(V1Routes(), optionalRoutes...)
	for _, route := range routes {
		operation := map[string]any{
			"operationId":                route.ID,
			"summary":                    humanizeRouteID(route.ID),
			"tags":                       []string{string(route.ActionClass)},
			"x-goobers-action-class":     route.ActionClass,
			"x-goobers-cost":             route.Cost,
			"x-goobers-budget-ms":        route.Budget.Milliseconds(),
			"x-goobers-recovery-safe":    route.RecoverySafe,
			"x-goobers-remote-invocable": InitiallyRemoteInvocable(route.ID),
			"responses":                  openAPIResponses(route),
		}
		if authenticated {
			operation["security"] = []map[string][]string{{"bearerAuth": {}}}
		} else {
			operation["security"] = []map[string][]string{}
		}
		if eventIngressRoute(route.ID) || workbenchProposalRoute(route.ID) || route.ID == RouteWorkbenchGraph || workbenchWriteRoute(route.ID) || workbenchReadRoute(route.ID) || sessionRoute(route.ID) || route.ID == RouteGaggleInteractiveCapabilities || route.ID == RouteInteractiveRun || route.ID == RouteInteractiveRunCommand || route.ID == RouteChildWorkflowMonitor || route.ID == RouteChildPublicationCheck {
			operation["security"] = []map[string][]string{{"bearerAuth": {}}}
		}
		if sessionOperationRoute(route.ID) {
			operation["security"] = []map[string][]string{{"bearerAuth": {}}}
			operation["x-goobers-session-grant"] = "live-turn"
		}
		if childWorkflowRoute(route.ID) {
			operation["security"] = []map[string][]string{{"bearerAuth": {}}}
			operation["x-goobers-stage-grant"] = "goobers-child"
		}
		if route.Capability != "" {
			operation["x-goobers-capability"] = route.Capability
		}
		if parameters := openAPIParameters(route); len(parameters) != 0 {
			operation["parameters"] = parameters
		}
		if body := openAPIRequestBody(route); body != nil {
			operation["requestBody"] = body
		}
		methods, ok := paths[route.Path].(map[string]any)
		if !ok {
			methods = map[string]any{}
			paths[route.Path] = methods
		}
		methods[strings.ToLower(route.Method)] = operation
	}

	document := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Goobers daemon API",
			"version":     "v1",
			"description": "Versioned daemon control and observation API. Use /.well-known/goobers for instance discovery.",
		},
		"servers": []map[string]any{{"url": "/"}},
		"paths":   paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"},
			},
			"schemas": openAPISchemas(authenticated),
		},
	}
	output, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal OpenAPI document: %w", err)
	}
	return append(output, '\n'), nil
}

func openAPIParameters(route Route) []map[string]any {
	names := pathParameterPattern.FindAllStringSubmatch(route.Path, -1)
	parameters := make([]map[string]any, 0, len(names)+2)
	for _, name := range names {
		parameters = append(parameters, map[string]any{
			"name": name[1], "in": "path", "required": true,
			"schema": map[string]any{"type": "string"},
		})
	}
	switch route.ID {
	case RouteGaggles, RouteGaggleGoobers, RouteGaggleWorkflows:
		parameters = append(parameters,
			map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}},
			map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}},
		)
	case RouteConfigSourceDocument:
		parameters = append(parameters,
			map[string]any{"name": "path", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
		)
	case RouteRuns:
		parameters = append(parameters,
			map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}},
			map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}},
		)
	}
	if route.ID == RouteGaggleEventPublish {
		parameters = append(parameters, map[string]any{"name": EventBindingHeader, "in": "header", "required": true, "schema": sessionString(128)})
	}
	parameters = append(parameters, openAPIServiceParameters(route.ID)...)
	parameters = append(parameters, sessionParameters(route.ID)...)
	parameters = append(parameters, workbenchReadParameters(route.ID)...)
	if route.ID == RouteRuns {
		for _, name := range []string{"gaggle", "workflow", "stage", "outcome", "population", "phase", "trigger"} {
			parameters = append(parameters, map[string]any{
				"name": name, "in": "query", "schema": map[string]any{"type": "string"},
			})
		}
		for _, name := range []string{"since", "until"} {
			parameters = append(parameters, map[string]any{
				"name": name, "in": "query", "schema": map[string]any{"type": "string", "format": "date-time"},
			})
		}
		for _, name := range []string{"latestPerWorkflow", "showNoWork", "orderByActivity"} {
			parameters = append(parameters, map[string]any{
				"name": name, "in": "query", "schema": map[string]any{"type": "boolean"},
			})
		}
	}
	if route.ID == RouteEvents {
		parameters = append(parameters, map[string]any{
			"name": "Last-Event-ID", "in": "header",
			"schema": map[string]any{"type": "string", "maxLength": 512},
		})
	}
	if route.ID == RouteChildWorkflowMonitor {
		parameters = append(parameters, map[string]any{"name": "after", "in": "query", "schema": map[string]any{"type": "string", "maxLength": 128}})
	}
	if route.ID == RouteRunRecovery || route.ID == RouteRunRecoveryPublish {
		for _, name := range []string{"repositoryKey", "issue"} {
			parameters = append(parameters, map[string]any{
				"name": name, "in": "query", "required": true,
				"schema": map[string]any{"type": "string", "minLength": 1},
			})
		}
	}
	if routeRequiresIdempotency(route.ID) {
		maxLength := 200
		if route.ID == RouteChildWorkflowStart {
			maxLength = MaxChildWorkflowInvocationKeyBytes
		}
		if route.ID == RouteTriggerIngest {
			maxLength = 128
		}
		parameters = append(parameters, map[string]any{
			"name": "Idempotency-Key", "in": "header", "required": true,
			"schema": map[string]any{"type": "string", "minLength": 1, "maxLength": maxLength},
		})
	}
	return parameters
}

func openAPIServiceParameters(id RouteID) []map[string]any {
	switch id {
	case RouteWorkItems:
		return []map[string]any{
			{"name": "provider", "in": "query", "schema": stringSchema()},
			{"name": "kind", "in": "query", "schema": map[string]any{"type": "string", "enum": []string{"pr", "issue"}}},
			{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}},
		}
	case RouteWorkItemDetail:
		return []map[string]any{
			{"name": "repository", "in": "query", "required": true, "schema": map[string]any{"type": "string", "minLength": 1}},
		}
	case RouteTelemetryDefectAggregates:
		return []map[string]any{
			{"name": "gaggle", "in": "query", "required": true, "schema": map[string]any{"type": "string", "minLength": 1}},
			{"name": "since", "in": "query", "required": true, "schema": dateTimeSchema(),
				"description": "Start of the bounded lookback window ending at request time; must satisfy the daemon's window policy."},
		}
	case RouteGaggleStatePut:
		return []map[string]any{
			{"name": "If-Match", "in": "header", "schema": map[string]any{"type": "string", "pattern": `^"[^",]+"$`},
				"description": "Replace this version. Exactly one of If-Match or If-None-Match is required; do not send both."},
			{"name": "If-None-Match", "in": "header", "schema": map[string]any{"type": "string", "const": "*"},
				"description": "Create only if absent. Exactly one of If-Match or If-None-Match is required; do not send both."},
		}
	}
	return nil
}

func routeRequiresIdempotency(id RouteID) bool {
	switch id {
	case RouteWorkbenchProposalSubmit, RouteWorkbenchPatch, RouteApproveStage, RouteOverrideStage, RouteRerunStage, RouteTriggerIngest,
		RouteResolveEscalation, RouteCancelRun, RouteOperatorMessageSubmit, RouteChildWorkflowStart, RouteInteractiveRunCommand, RouteChildPublicationCheck, RouteSessionCreate, RouteSessionMessage, RouteSessionClose:
		return true
	default:
		return false
	}
}

func openAPIRequestBody(route Route) map[string]any {
	if route.ID == RouteGaggleEventPublish {
		return map[string]any{"required": true, "content": map[string]any{"application/cloudevents+json": map[string]any{"schema": schemaRef("GaggleEventEnvelope")}, "application/json": map[string]any{"schema": schemaRef("GaggleEventEnvelope")}}}
	}
	if sessionOperationRoute(route.ID) {
		return sessionOperationBody(route.ID)
	}
	if route.Method == http.MethodGet || route.Method == http.MethodHead {
		return nil
	}
	if workbenchProposalRoute(route.ID) {
		return workbenchProposalRequestBody(route.ID)
	}
	if route.ID == RouteBlobPut || route.ID == RouteRunRecoveryPublish {
		return map[string]any{
			"required": true,
			"content": map[string]any{
				"application/octet-stream": map[string]any{
					"schema": map[string]any{"type": "string", "format": "binary"},
				},
			},
		}
	}
	schema := map[string]any{"type": "object", "additionalProperties": true}
	switch route.ID {
	case RouteWorkbenchPatch:
		schema = schemaRef("BacklogPatchInput")
	case RouteChildWorkflowAccessAcquire, RouteChildWorkflowAccessRevoke:
		schema = closedChildObject([]string{"contractDigest"}, map[string]any{"contractDigest": map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"}})
	case RouteChildWorkflowValidate, RouteChildWorkflowStart:
		schema = schemaRef("ChildWorkflowSourceRequest")
	case RouteChildWorkflowStatus:
		schema = schemaRef("ChildWorkflowStatusRequest")
	case RouteChildWorkflowResolve:
		schema = schemaRef("ChildWorkflowResolveRequest")
	case RouteTriggerIngest:
		schema = schemaRef("TriggerRequest")
	case RouteCancelRun:
		schema = schemaRef("CancelRunRequest")
	case RouteChildPublicationCheck:
		schema = schemaRef("ChildPublicationCheckRequest")
	case RouteSessionCreate:
		schema = schemaRef("SessionCreateRequest")
	case RouteSessionMessage:
		schema = schemaRef("SessionMessageRequest")
	case RouteSessionClose:
		schema = schemaRef("SessionCloseRequest")
	case RouteInteractiveRunCommand:
		schema = schemaRef("InteractiveRunCommand")
	case RouteOperatorMessageSubmit:
		schema = schemaRef("OperatorMessageSubmitRequest")
	case RouteApproveStage, RouteOverrideStage, RouteRerunStage:
		schema = schemaRef("InterventionRequest")
	case RouteResolveEscalation:
		schema = schemaRef("EscalationResolutionRequest")
	case RouteWorkflowEnabled:
		schema = schemaRef("WorkflowEnabledRequest")
	case RouteGaggleBundleImport:
		schema = schemaRef("GaggleBundleImportRequest")
	}
	return map[string]any{
		"required": true,
		"content": map[string]any{
			"application/json": map[string]any{"schema": schema},
		},
	}
}

func openAPIResponses(route Route) map[string]any {
	if eventIngressRoute(route.ID) {
		return eventIngressResponses(route.ID)
	}
	if response := workbenchResponses(route.ID); response != nil {
		return response
	}
	if sessionOperationRoute(route.ID) {
		return sessionOperationResponses(route.ID)
	}
	if sessionRoute(route.ID) {
		return sessionResponses(route)
	}
	switch route.ID {
	case RouteChildWorkflowAccessAcquire:
		return map[string]any{"200": jsonResponse("Secret delivery to an authenticated parent attempt; never cache or persist", closedChildObject([]string{"endpoint", "bearerToken"}, map[string]any{"endpoint": stringSchema(), "bearerToken": map[string]any{"type": "string", "format": "password"}})), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}

	case RouteOpenAPI:
		return map[string]any{
			"200":     metadataResponse("OpenAPI 3.1 document", "application/vnd.oai.openapi+json;version=3.1", map[string]any{"type": "object"}),
			"default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope")),
		}
	case RouteEvents:
		return map[string]any{
			"200": mediaResponseWithHeaders(
				"Server-sent event stream",
				"text/event-stream",
				map[string]any{"type": "string"},
				responseHeaders("Cache-Control", "X-Accel-Buffering"),
			),
			"default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope")),
		}
	case RouteBlobGet, RouteRunRecovery, RouteRunArtifact:
		return map[string]any{
			"200":     mediaResponse("Binary content", "application/octet-stream", map[string]any{"type": "string", "format": "binary"}),
			"default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope")),
		}
	case RouteRunTranscript:
		return map[string]any{
			"200":     mediaResponse("Transcript text", "text/plain", map[string]any{"type": "string"}),
			"default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope")),
		}
	}
	successSchema := map[string]any{"type": "object", "additionalProperties": true}
	switch route.ID {
	case RouteInteractiveRun:
		successSchema = schemaRef("InteractiveRunView")
	case RouteChildPublicationCheck:
		successSchema = schemaRef("ChildPublicationCheckResult")
	case RouteChildWorkflowMonitor:
		successSchema = schemaRef("ChildWorkflowPage")
	case RouteInteractiveRunCommand:
		successSchema = schemaRef("InteractiveRunCommandResult")
	case RouteGaggleInteractiveCapabilities:
		successSchema = schemaRef("InteractiveCapabilities")
	case RouteChildWorkflowValidate:
		successSchema = schemaRef("ChildWorkflowValidationResponse")
	case RouteChildWorkflowStart, RouteChildWorkflowStatus:
		successSchema = schemaRef("ChildWorkflowResponse")
	case RouteChildWorkflowResolve:
		successSchema = schemaRef("ChildWorkflowResolutionResponse")
	case RouteDiscovery:
		successSchema = schemaRef("DiscoveryDocument")
	case RouteCapabilities:
		successSchema = schemaRef("CapabilityDocument")
	case RouteHealth:
		successSchema = schemaRef("HealthDocument")
	case RouteInstance:
		successSchema = schemaRef("InstanceDocument")
	case RouteRuns:
		successSchema = schemaRef("RunListDocument")
	case RouteInstanceReadiness:
		successSchema = schemaRef("InstanceReadiness")
	case RouteTriggerIngest:
		successSchema = schemaRef("TriggerResponse")
	case RouteTriggerStatus:
		successSchema = schemaRef("TriggerStatusResponse")
	case RouteCancelRun:
		successSchema = schemaRef("CancelRunResult")
	case RouteOperatorMessageSubmit:
		successSchema = schemaRef("OperatorMessageSubmitResponse")
	case RouteApproveStage, RouteOverrideStage, RouteRerunStage, RouteResolveEscalation:
		successSchema = schemaRef("InterventionResult")
	case RouteWorkflowEnabled:
		successSchema = schemaRef("WorkflowEnabledResult")
	case RouteGaggleBundleExport:
		successSchema = schemaRef("GaggleBundle")
	case RouteGaggleBundleImport:
		successSchema = schemaRef("GaggleBundleImportResult")
	}
	successResponse := jsonResponse("Successful response", successSchema)
	if route.ID == RouteDiscovery || route.ID == RouteCapabilities {
		successResponse = metadataResponse("Successful response", "application/json", successSchema)
	}
	responses := map[string]any{
		"200":     successResponse,
		"default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope")),
	}
	if childWorkflowRoute(route.ID) {
		if route.ID == RouteChildWorkflowStart || route.ID == RouteChildWorkflowResolve {
			delete(responses, "200")
			responses["202"] = jsonResponse("Durable custody accepted; execution may still be queued", successSchema)
		}
		return responses
	}
	if route.Method != http.MethodGet && route.Method != http.MethodHead {
		responses["202"] = jsonResponse("Request accepted", successSchema)
		responses["204"] = map[string]any{"description": "Request completed without a response body"}
	}
	return responses
}

func jsonResponse(description string, schema map[string]any) map[string]any {
	return mediaResponse(description, "application/json", schema)
}

func mediaResponse(description, contentType string, schema map[string]any) map[string]any {
	return mediaResponseWithHeaders(description, contentType, schema, nil)
}

func mediaResponseWithHeaders(description, contentType string, schema, headers map[string]any) map[string]any {
	response := map[string]any{
		"description": description,
		"content": map[string]any{
			contentType: map[string]any{"schema": schema},
		},
	}
	if len(headers) != 0 {
		response["headers"] = headers
	}
	return response
}

func responseHeaders(names ...string) map[string]any {
	headers := make(map[string]any, len(names))
	for _, name := range names {
		headers[name] = map[string]any{"schema": map[string]any{"type": "string"}}
	}
	return headers
}

func metadataResponse(description, contentType string, schema map[string]any) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			contentType: map[string]any{"schema": schema},
		},
		"headers": responseHeaders("Cache-Control", "Content-Encoding", "ETag", "Vary"),
	}
}

func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func cloneSchemaProperties(source map[string]any) map[string]any {
	return mergeSchemaProperties(source, nil)
}

func mergeSchemaProperties(left, right map[string]any) map[string]any {
	merged := make(map[string]any, len(left)+len(right))
	for key, value := range left {
		merged[key] = value
	}
	for key, value := range right {
		merged[key] = value
	}
	return merged
}

func openAPISchemas(authenticated bool) map[string]any {
	return mergeSchemaProperties(
		mergeSchemaProperties(openAPIDiscoverySchemas(), openAPIRemoteReadSchemas()),
		mergeSchemaProperties(openAPIOperationSchemas(authenticated), mergeSchemaProperties(openAPIChildWorkflowSchemas(), openAPIInteractiveSchemas())),
	)
}

func openAPIDiscoverySchemas() map[string]any {
	routeCapability := map[string]any{
		"type":     "object",
		"required": []string{"id", "method", "path", "actionClass", "requiredRole", "remoteInvocable", "available", "streaming", "recoverySafe"},
		"properties": map[string]any{
			"id":              map[string]any{"type": "string"},
			"method":          map[string]any{"type": "string"},
			"path":            map[string]any{"type": "string"},
			"actionClass":     map[string]any{"type": "string"},
			"capability":      map[string]any{"type": "string"},
			"requiredRole":    map[string]any{"type": "string", "enum": []string{"view", "operate"}},
			"remoteInvocable": map[string]any{"type": "boolean"},
			"available":       map[string]any{"type": "boolean"},
			"code":            map[string]any{"type": "string"},
			"reason":          map[string]any{"type": "string"},
			"streaming":       map[string]any{"type": "boolean"},
			"recoverySafe":    map[string]any{"type": "boolean"},
		},
	}
	protocolIdentityProperties := map[string]any{
		"daemonProtocolVersion": map[string]any{"type": "integer", "const": DaemonProtocolVersion},
		"daemonInstanceId":      map[string]any{"type": "string", "minLength": 1},
		"daemonBootId":          map[string]any{"type": "string", "format": "uuid"},
		"preferredApiVersion":   map[string]any{"type": "string", "const": PreferredAPIVersion},
	}
	protocolSummaryProperties := cloneSchemaProperties(protocolIdentityProperties)
	protocolSummaryProperties["openapiSha256"] = map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}
	protocolSummaryProperties["capabilitiesEtag"] = map[string]any{"type": "string", "minLength": 2}
	discoveryLink := map[string]any{
		"type":     "object",
		"required": []string{"href"},
		"properties": map[string]any{
			"href":   map[string]any{"type": "string", "pattern": "^/"},
			"sha256": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"},
			"etag":   map[string]any{"type": "string"},
		},
		"additionalProperties": false,
	}
	return map[string]any{
		"DiscoveryDocument": map[string]any{
			"type":     "object",
			"required": []string{"product", "daemonProtocolVersion", "daemonInstanceId", "daemonBootId", "daemonVersion", "authentication", "preferredApiVersion", "apiVersions", "openapiSha256", "capabilitiesEtag", "links", "apis"},
			"properties": mergeSchemaProperties(protocolSummaryProperties, map[string]any{
				"product":        map[string]any{"type": "string", "const": "goobers"},
				"daemonVersion":  map[string]any{"type": "string"},
				"daemonCommit":   map[string]any{"type": "string"},
				"authentication": map[string]any{"type": "string", "enum": []string{"none", "bearer", "disabled"}},
				"apiVersions":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "uniqueItems": true},
				"links": map[string]any{
					"type":     "object",
					"required": []string{"instance"},
					"properties": map[string]any{
						"instance": discoveryLink,
					},
					"additionalProperties": false,
				},
				"apis": map[string]any{
					"type":     "object",
					"required": []string{PreferredAPIVersion},
					"properties": map[string]any{
						PreferredAPIVersion: map[string]any{
							"type":     "object",
							"required": []string{"openapi", "capabilities", "health", "readiness"},
							"properties": map[string]any{
								"openapi":      discoveryLink,
								"capabilities": discoveryLink,
								"health":       discoveryLink,
								"readiness":    discoveryLink,
							},
							"additionalProperties": false,
						},
					},
				},
			}),
			"additionalProperties": true,
		},
		"CapabilityDocument": map[string]any{
			"type":     "object",
			"required": []string{"daemonProtocolVersion", "daemonInstanceId", "daemonBootId", "preferredApiVersion", "apiVersion", "schemaVersion", "openapiSha256", "routes"},
			"properties": mergeSchemaProperties(protocolIdentityProperties, map[string]any{
				"apiVersion":    map[string]any{"type": "string", "const": PreferredAPIVersion},
				"schemaVersion": map[string]any{"type": "integer", "const": 1},
				"openapiSha256": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"},
				"routes":        map[string]any{"type": "array", "items": routeCapability},
			}),
			"additionalProperties": true,
		},
		"ProtocolSummary": map[string]any{
			"type":                 "object",
			"required":             []string{"daemonProtocolVersion", "daemonInstanceId", "daemonBootId", "preferredApiVersion", "openapiSha256", "capabilitiesEtag"},
			"properties":           protocolSummaryProperties,
			"additionalProperties": false,
		},
		"HealthDocument": map[string]any{
			"type":     "object",
			"required": []string{"apiVersion", "schemaVersion", "build", "ready", "healthy", "instance", "freshness", "protocol"},
			"properties": map[string]any{
				"apiVersion":       map[string]any{"type": "string", "const": PreferredAPIVersion},
				"schemaVersion":    map[string]any{"type": "string"},
				"build":            schemaRef("BuildMetadata"),
				"ready":            map[string]any{"type": "boolean"},
				"healthy":          map[string]any{"type": "boolean"},
				"instance":         schemaRef("InstanceIdentity"),
				"freshness":        schemaRef("Freshness"),
				"definitionReload": map[string]any{"type": "object", "additionalProperties": true},
				"startup":          map[string]any{"type": "object", "additionalProperties": true},
				"update":           map[string]any{"type": "object", "additionalProperties": true},
				"readState":        map[string]any{"type": "object", "additionalProperties": true},
				"protocol":         schemaRef("ProtocolSummary"),
			},
			"additionalProperties": true,
		},
		"InstanceReadiness": map[string]any{
			"type":     "object",
			"required": []string{"apiVersion", "schemaVersion", "instanceRoot", "ready", "recovery", "protocol"},
			"properties": map[string]any{
				"apiVersion":    map[string]any{"type": "string", "const": PreferredAPIVersion},
				"schemaVersion": map[string]any{"type": "string"},
				"computerName":  map[string]any{"type": "string"},
				"instanceRoot":  map[string]any{"type": "string"},
				"rootIdentity":  map[string]any{"type": "object", "additionalProperties": true},
				"ready":         map[string]any{"type": "boolean"},
				"recovery": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"phase":             map[string]any{"type": "string"},
						"target":            map[string]any{"type": "string"},
						"elapsedSeconds":    map[string]any{"type": "number", "minimum": 0},
						"worktreeCount":     map[string]any{"type": "integer", "minimum": 0},
						"recoveryRunCount":  map[string]any{"type": "integer", "minimum": 0},
						"accumulationCount": map[string]any{"type": "integer", "minimum": 0},
						"budgetSeconds":     map[string]any{"type": "number", "minimum": 0},
						"budgetUsedPercent": map[string]any{"type": "number", "minimum": 0},
						"budgetState": map[string]any{
							"type": "string",
							"enum": []string{"unavailable", "within-budget", "approaching", "exceeded"},
						},
					},
					"additionalProperties": true,
				},
				"protocol": schemaRef("ProtocolSummary"),
			},
			"additionalProperties": true,
		},
	}
}

func openAPIOperationSchemas(authenticated bool) map[string]any {
	actorRequired := []string{}
	if !authenticated {
		actorRequired = append(actorRequired, "actor")
	}
	actor := map[string]any{
		"type":        "string",
		"description": "Required in local-trust mode. In authenticated deployments the principal supplies the actor and overrides this value.",
	}
	return map[string]any{
		"TriggerRequest": map[string]any{
			"type": "object", "required": []string{"workflow"}, "additionalProperties": false,
			"properties": map[string]any{
				"gaggle":    map[string]any{"type": "string"},
				"workflow":  map[string]any{"type": "string", "minLength": 1},
				"requestId": map[string]any{"type": "string"},
				"force":     map[string]any{"type": "boolean"},
				"sourceRun": map[string]any{"type": "string"},
			},
		},
		"TriggerResponse": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"acceptanceId": map[string]any{"type": "string"},
				"state":        map[string]any{"type": "string"},
				"runId":        map[string]any{"type": "string"},
				"duplicate":    map[string]any{"type": "boolean"},
			},
		},
		"TriggerStatusResponse": map[string]any{
			"type": "object", "required": []string{"acceptanceId", "state", "acceptedAt"},
			"properties": map[string]any{
				"acceptanceId": map[string]any{"type": "string"},
				"state":        map[string]any{"type": "string"},
				"runId":        map[string]any{"type": "string"},
				"reason":       map[string]any{"type": "string"},
				"acceptedAt":   map[string]any{"type": "string", "format": "date-time"},
			},
		},
		"CancelRunRequest": map[string]any{
			"type": "object", "required": actorRequired, "additionalProperties": false,
			"properties": map[string]any{
				"actor":    actor,
				"workflow": map[string]any{"type": "string"},
				"gaggle":   map[string]any{"type": "string"},
			},
		},
		"CancelRunResult": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"phase": map[string]any{"type": "string"},
				"code":  map[string]any{"type": "string"},
				"error": map[string]any{"type": "string"},
			},
		},
		"OperatorMessageSubmitRequest": map[string]any{
			"type": "object", "required": []string{"gaggle", "targetAddress", "purpose", "content", "deliveryMode"}, "additionalProperties": false,
			"properties": map[string]any{
				"gaggle":        map[string]any{"type": "string", "minLength": 1},
				"targetAddress": map[string]any{"type": "string", "minLength": 1},
				"expiresAt":     map[string]any{"type": "string", "format": "date-time"},
				"purpose":       map[string]any{"type": "string", "minLength": 1},
				"content": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"text":     map[string]any{"type": "string"},
						"artifact": map[string]any{"type": "object", "additionalProperties": true},
					},
				},
				"deliveryMode": map[string]any{"type": "string", "minLength": 1},
			},
		},
		"OperatorMessageSubmitResponse": map[string]any{
			"type": "object", "required": []string{"accepted", "record"},
			"properties": map[string]any{
				"accepted": map[string]any{"type": "boolean"},
				"record":   map[string]any{"type": "object", "additionalProperties": true},
			},
		},
		"InterventionRequest": map[string]any{
			"type": "object", "required": actorRequired, "additionalProperties": false,
			"properties": map[string]any{
				"actor":               actor,
				"decision":            map[string]any{"type": "string"},
				"rationale":           map[string]any{"type": "string"},
				"instructionAddendum": map[string]any{"type": "string"},
			},
		},
		"EscalationResolutionRequest": map[string]any{
			"type": "object", "required": append([]string{"resolution"}, actorRequired...), "additionalProperties": false,
			"properties": map[string]any{
				"actor":      actor,
				"resolution": map[string]any{"type": "string", "enum": []string{"approve", "deny", "redirect"}},
				"gate":       map[string]any{"type": "string"},
				"decision":   map[string]any{"type": "string"},
				"rationale":  map[string]any{"type": "string"},
			},
		},
		"InterventionResult": map[string]any{
			"type": "object", "required": []string{"phase", "journalSeq"},
			"properties": map[string]any{
				"phase":      map[string]any{"type": "string"},
				"state":      map[string]any{"type": "string"},
				"journalSeq": map[string]any{"type": "integer", "minimum": 1},
			},
		},
		"WorkflowEnabledRequest": map[string]any{
			"type": "object", "required": []string{"enabled"}, "additionalProperties": false,
			"properties": map[string]any{"enabled": map[string]any{"type": "boolean"}},
		},
		"WorkflowEnabledResult": map[string]any{
			"type": "object", "required": []string{"gaggle", "workflow", "enabled"},
			"properties": map[string]any{
				"gaggle":   map[string]any{"type": "string"},
				"workflow": map[string]any{"type": "string"},
				"enabled":  map[string]any{"type": "boolean"},
			},
		},
		"GaggleBundleSource": map[string]any{
			"type": "object", "required": []string{"name", "apiVersion", "digest"}, "additionalProperties": false,
			"properties": map[string]any{
				"name":       map[string]any{"type": "string", "minLength": 1},
				"apiVersion": map[string]any{"type": "string", "minLength": 1},
				"digest":     map[string]any{"type": "string", "pattern": "^sha256:[a-f0-9]{64}$"},
			},
		},
		"GaggleBundle": map[string]any{
			"type":                 "object",
			"required":             []string{"apiVersion", "kind", "schemaVersion", "source", "exportedAt", "provenance", "definition", "digest"},
			"additionalProperties": false,
			"properties": map[string]any{
				"apiVersion":    map[string]any{"type": "string", "const": "goobers.dev/v1alpha1"},
				"kind":          map[string]any{"type": "string", "const": "GaggleBundle"},
				"schemaVersion": map[string]any{"type": "integer", "const": 1},
				"source":        schemaRef("GaggleBundleSource"),
				"exportedAt":    map[string]any{"type": "string", "format": "date-time"},
				"provenance":    map[string]any{"type": "object", "additionalProperties": true},
				"definition":    map[string]any{"type": "object", "additionalProperties": true},
				"digest":        map[string]any{"type": "string", "pattern": "^sha256:[a-f0-9]{64}$"},
			},
		},
		"GaggleBundleImportRequest": map[string]any{
			"type": "object", "required": []string{"name", "bundle"}, "additionalProperties": false,
			"properties": map[string]any{
				"name":   map[string]any{"type": "string", "pattern": "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"},
				"bundle": schemaRef("GaggleBundle"),
			},
		},
		"GaggleBundleImportResult": map[string]any{
			"type": "object", "required": []string{"name", "source", "importedAt", "restartRequired"}, "additionalProperties": false,
			"properties": map[string]any{
				"name":            map[string]any{"type": "string"},
				"source":          schemaRef("GaggleBundleSource"),
				"importedAt":      map[string]any{"type": "string", "format": "date-time"},
				"restartRequired": map[string]any{"type": "boolean"},
			},
		},
		"ErrorEnvelope": map[string]any{
			"type": "object", "required": []string{"error"},
			"properties": map[string]any{
				"error": map[string]any{
					"type": "object", "required": []string{"code", "message"},
					"properties": map[string]any{
						"code":    map[string]any{"type": "string"},
						"message": map[string]any{"type": "string"},
						"details": map[string]any{},
					},
				},
			},
		},
	}
}

func humanizeRouteID(id RouteID) string {
	value := string(id)
	var words []string
	start := 0
	for index := 1; index < len(value); index++ {
		if value[index] >= 'A' && value[index] <= 'Z' {
			words = append(words, value[start:index])
			start = index
		}
	}
	words = append(words, value[start:])
	for index := range words {
		words[index] = strings.ToLower(words[index])
	}
	return strings.ToUpper(words[0][:1]) + words[0][1:] + " " + strings.Join(words[1:], " ")
}
