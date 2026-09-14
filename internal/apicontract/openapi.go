package apicontract

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

var pathParameterPattern = regexp.MustCompile(`\{([^}]+)\}`)

// OpenAPIDocument renders the daemon's machine-readable HTTP contract.
func OpenAPIDocument(authenticated bool) ([]byte, error) {
	paths := map[string]any{}
	for _, route := range V1Routes() {
		operation := map[string]any{
			"operationId":             route.ID,
			"summary":                 humanizeRouteID(route.ID),
			"tags":                    []string{string(route.ActionClass)},
			"x-goobers-action-class":  route.ActionClass,
			"x-goobers-cost":          route.Cost,
			"x-goobers-budget-ms":     route.Budget.Milliseconds(),
			"x-goobers-recovery-safe": route.RecoverySafe,
			"responses":               openAPIResponses(route),
		}
		if authenticated {
			operation["security"] = []map[string][]string{{"bearerAuth": {}}}
		} else {
			operation["security"] = []map[string][]string{}
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
		"servers": []map[string]any{{"url": "."}},
		"paths":   paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"},
			},
			"schemas": openAPISchemas(),
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
	case RouteRuns, RouteWorkItems:
		parameters = append(parameters,
			map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}},
			map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}},
		)
	}
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

func routeRequiresIdempotency(id RouteID) bool {
	switch id {
	case RouteApproveStage, RouteOverrideStage, RouteRerunStage, RouteTriggerIngest,
		RouteResolveEscalation, RouteCancelRun:
		return true
	default:
		return false
	}
}

func openAPIRequestBody(route Route) map[string]any {
	if route.Method == http.MethodGet || route.Method == http.MethodHead {
		return nil
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
	case RouteTriggerIngest:
		schema = schemaRef("TriggerRequest")
	case RouteCancelRun:
		schema = schemaRef("CancelRunRequest")
	case RouteApproveStage, RouteOverrideStage, RouteRerunStage:
		schema = schemaRef("InterventionRequest")
	case RouteResolveEscalation:
		schema = schemaRef("EscalationResolutionRequest")
	case RouteWorkflowEnabled:
		schema = schemaRef("WorkflowEnabledRequest")
	}
	return map[string]any{
		"required": true,
		"content": map[string]any{
			"application/json": map[string]any{"schema": schema},
		},
	}
}

func openAPIResponses(route Route) map[string]any {
	switch route.ID {
	case RouteOpenAPI:
		return map[string]any{
			"200":     mediaResponse("OpenAPI 3.1 document", "application/vnd.oai.openapi+json", map[string]any{"type": "object"}),
			"default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope")),
		}
	case RouteEvents:
		return map[string]any{
			"200":     mediaResponse("Server-sent event stream", "text/event-stream", map[string]any{"type": "string"}),
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
	case RouteDiscovery:
		successSchema = schemaRef("DiscoveryDocument")
	case RouteCapabilities:
		successSchema = schemaRef("CapabilityDocument")
	case RouteTriggerIngest:
		successSchema = schemaRef("TriggerResponse")
	case RouteTriggerStatus:
		successSchema = schemaRef("TriggerStatusResponse")
	case RouteCancelRun:
		successSchema = schemaRef("CancelRunResult")
	case RouteApproveStage, RouteOverrideStage, RouteRerunStage, RouteResolveEscalation:
		successSchema = schemaRef("InterventionResult")
	case RouteWorkflowEnabled:
		successSchema = schemaRef("WorkflowEnabledResult")
	}
	responses := map[string]any{
		"200":     jsonResponse("Successful response", successSchema),
		"default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope")),
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
	return map[string]any{
		"description": description,
		"content": map[string]any{
			contentType: map[string]any{"schema": schema},
		},
	}
}

func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func openAPISchemas() map[string]any {
	routeCapability := map[string]any{
		"type":     "object",
		"required": []string{"id", "method", "path", "actionClass", "requiredRole", "available", "streaming", "recoverySafe"},
		"properties": map[string]any{
			"id":           map[string]any{"type": "string"},
			"method":       map[string]any{"type": "string"},
			"path":         map[string]any{"type": "string"},
			"actionClass":  map[string]any{"type": "string"},
			"capability":   map[string]any{"type": "string"},
			"requiredRole": map[string]any{"type": "string", "enum": []string{"view", "operate"}},
			"available":    map[string]any{"type": "boolean"},
			"reason":       map[string]any{"type": "string"},
			"streaming":    map[string]any{"type": "boolean"},
			"recoverySafe": map[string]any{"type": "boolean"},
		},
	}
	return map[string]any{
		"DiscoveryDocument": map[string]any{
			"type":     "object",
			"required": []string{"product", "daemonVersion", "authentication", "preferredApiVersion", "apiVersions", "openapi", "capabilities", "instance", "health", "openapiSha256"},
			"properties": map[string]any{
				"product":             map[string]any{"type": "string", "const": "goobers"},
				"daemonVersion":       map[string]any{"type": "string"},
				"daemonCommit":        map[string]any{"type": "string"},
				"authentication":      map[string]any{"type": "string", "enum": []string{"none", "bearer", "disabled"}},
				"preferredApiVersion": map[string]any{"type": "string", "const": "v1"},
				"apiVersions":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"openapi":             map[string]any{"type": "string"},
				"capabilities":        map[string]any{"type": "string"},
				"instance":            map[string]any{"type": "string"},
				"health":              map[string]any{"type": "string"},
				"openapiSha256":       map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"},
			},
		},
		"CapabilityDocument": map[string]any{
			"type":     "object",
			"required": []string{"apiVersion", "schemaVersion", "openapiSha256", "routes"},
			"properties": map[string]any{
				"apiVersion":    map[string]any{"type": "string", "const": "v1"},
				"schemaVersion": map[string]any{"type": "integer", "const": 1},
				"openapiSha256": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"},
				"routes":        map[string]any{"type": "array", "items": routeCapability},
			},
		},
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
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
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
		"InterventionRequest": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"decision":            map[string]any{"type": "string"},
				"rationale":           map[string]any{"type": "string"},
				"instructionAddendum": map[string]any{"type": "string"},
			},
		},
		"EscalationResolutionRequest": map[string]any{
			"type": "object", "required": []string{"resolution"}, "additionalProperties": false,
			"properties": map[string]any{
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
