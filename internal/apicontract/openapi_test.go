package apicontract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestOpenAPIMutationActorMatchesAuthenticationMode(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		document, err := OpenAPIDocument(authenticated)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Components struct {
				Schemas map[string]json.RawMessage `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(document, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"CancelRunRequest", "InterventionRequest", "EscalationResolutionRequest"} {
			schema, err := jsonschema.CompileString("request.json", string(decoded.Components.Schemas[name]))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			request := map[string]any{"actor": "local-operator"}
			if name == "EscalationResolutionRequest" {
				request["resolution"] = "approve"
			}
			if err := schema.Validate(request); err != nil {
				t.Errorf("%s authenticated=%t rejects actor-bearing request: %v", name, authenticated, err)
			}
			delete(request, "actor")
			if err := schema.Validate(request); (err == nil) != authenticated {
				t.Errorf("%s authenticated=%t missing actor validation = %v", name, authenticated, err)
			}
			request["unknown"] = true
			if err := schema.Validate(request); err == nil {
				t.Errorf("%s accepts an unknown property", name)
			}
		}
	}
}

func TestOpenAPIDocumentIsDeterministic(t *testing.T) {
	first, err := OpenAPIDocument(true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenAPIDocument(true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("OpenAPI generation changed bytes without a contract change")
	}
}

func TestOpenAPIDocumentDescribesEveryDaemonRoute(t *testing.T) {
	document, err := OpenAPIDocument(true)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		OpenAPI string `json:"openapi"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Paths map[string]map[string]operation `json:"paths"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q, want 3.1.0", decoded.OpenAPI)
	}
	if len(decoded.Servers) != 1 || decoded.Servers[0].URL != "/" {
		t.Fatalf("servers = %+v, want origin root", decoded.Servers)
	}
	operationCount := 0
	for _, methods := range decoded.Paths {
		operationCount += len(methods)
	}
	if operationCount != len(V1Routes()) {
		t.Fatalf("OpenAPI operations = %d, registered routes = %d", operationCount, len(V1Routes()))
	}
	for _, route := range V1Routes() {
		methods, ok := decoded.Paths[route.Path]
		if !ok {
			t.Errorf("path %q is missing", route.Path)
			continue
		}

		operation, ok := methods[lowerMethod(route.Method)]
		if !ok {
			t.Errorf("%s %s is missing", route.Method, route.Path)
			continue
		}
		if operation.OperationID != route.ID {
			t.Errorf("%s %s operationId = %q, want %q", route.Method, route.Path, operation.OperationID, route.ID)
		}
		if len(operation.Security) != 1 {
			t.Errorf("%s %s security = %+v, want bearer requirement", route.Method, route.Path, operation.Security)
		}
	}
}

func TestOpenAPIInitialRemoteProfileHasConcreteSchemasAndHeaders(t *testing.T) {
	document, err := OpenAPIDocument(true)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Paths map[string]map[string]struct {
			Parameters []openAPIParameterTest `json:"parameters"`
			Responses  map[string]struct {
				Content map[string]struct {
					Schema map[string]any `json:"schema"`
				} `json:"content"`
				Headers map[string]any `json:"headers"`
			} `json:"responses"`
			Remote bool `json:"x-goobers-remote-invocable"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, route := range V1Routes() {
		operation := decoded.Paths[route.Path][lowerMethod(route.Method)]
		if operation.Remote != InitiallyRemoteInvocable(route.ID) {
			t.Errorf("%s remote profile = %t", route.ID, operation.Remote)
		}
		if !InitiallyRemoteInvocable(route.ID) {
			continue
		}
		response := operation.Responses["200"]
		mediaType := "application/json"
		if route.ID == RouteEvents {
			mediaType = "text/event-stream"
		}
		schema := response.Content[mediaType].Schema
		if route.ID != RouteEvents {
			reference, ok := schema["$ref"].(string)
			if !ok || reference == "" {
				t.Errorf("%s response schema = %#v, want concrete component reference", route.ID, schema)
			}
		} else {
			if response.Headers["Cache-Control"] == nil || response.Headers["X-Accel-Buffering"] == nil {
				t.Errorf("events response headers = %#v", response.Headers)
			}
			foundCursor := false
			for _, parameter := range operation.Parameters {
				if parameter.Name == "Last-Event-ID" && parameter.In == "header" {
					foundCursor = true
				}
				if !foundCursor {
					t.Error("events operation is missing Last-Event-ID")
				}
			}
		}
	}
}

func TestOpenAPIDocumentUsesOnlyInternalSchemaReferences(t *testing.T) {
	document, err := OpenAPIDocument(true)
	if err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "$ref" {
					reference, ok := child.(string)
					if !ok || !strings.HasPrefix(reference, "#/components/schemas/") {
						t.Errorf("unsafe schema reference = %#v", child)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	var decoded any
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	walk(decoded)
}

func TestOpenAPIDocumentDescribesTriggerAdmission(t *testing.T) {
	document, err := OpenAPIDocument(false)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Paths map[string]map[string]operation `json:"paths"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	trigger := decoded.Paths[TriggerIngestPath]["post"]
	if len(trigger.Security) != 0 {
		t.Fatalf("local-trust security = %+v, want no requirement", trigger.Security)
	}
	if trigger.RequestBody == nil {
		t.Fatal("trigger request body is missing")
	}
	foundKey := false
	for _, parameter := range trigger.Parameters {
		if parameter.Name == "Idempotency-Key" && parameter.In == "header" && parameter.Required {
			foundKey = true
		}
	}
	if !foundKey {
		t.Fatal("trigger Idempotency-Key header is missing")
	}
}

func TestOpenAPIDocumentMarksEveryIdempotentMutation(t *testing.T) {
	document, err := OpenAPIDocument(true)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Paths map[string]map[string]operation `json:"paths"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, route := range V1Routes() {
		if !routeRequiresIdempotency(route.ID) {
			continue
		}
		operation := decoded.Paths[route.Path][lowerMethod(route.Method)]
		found := false
		for _, parameter := range operation.Parameters {
			if parameter.Name == "Idempotency-Key" && parameter.Required {
				found = true
			}
		}
		if !found {
			t.Errorf("%s has no required Idempotency-Key parameter", route.ID)
		}
	}
}

type operation struct {
	OperationID RouteID                `json:"operationId"`
	Security    []map[string][]string  `json:"security"`
	Parameters  []openAPIParameterTest `json:"parameters"`
	RequestBody map[string]any         `json:"requestBody"`
}

type openAPIParameterTest struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
}

func lowerMethod(method string) string {
	switch method {
	case "GET":
		return "get"
	case "POST":
		return "post"
	case "PUT":
		return "put"
	case "DELETE":
		return "delete"
	default:
		return method
	}
}
