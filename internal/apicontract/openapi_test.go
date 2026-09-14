package apicontract

import (
	"encoding/json"
	"testing"
)

func TestOpenAPIDocumentDescribesEveryDaemonRoute(t *testing.T) {
	document, err := OpenAPIDocument(true)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		OpenAPI string                          `json:"openapi"`
		Paths   map[string]map[string]operation `json:"paths"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q, want 3.1.0", decoded.OpenAPI)
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
