package apicontract

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestPRRepairRecoveryContractPreservesSeparateAcknowledgement(t *testing.T) {
	schemas := mergeSchemaProperties(openAPISessionSchemas(), openAPIPRRepairSchemas())
	raw, _ := json.Marshal(map[string]any{"components": map[string]any{"schemas": schemas}})
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("repair.json", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("repair.json#/components/schemas/SessionPRRepairCommand")
	if err != nil {
		t.Fatal(err)
	}
	value := prRepairRecoveryFixture(time.Now())
	encoded, _ := json.Marshal(value)
	var object map[string]any
	_ = json.Unmarshal(encoded, &object)
	if err = schema.Validate(object); err != nil {
		t.Fatal(err)
	}
	receipt := object["receipt"].(map[string]any)
	if receipt["outcome"] != "unknown" || receipt["providerAcknowledged"] != false {
		t.Fatal("observation rewrote acknowledgement")
	}
	observations := object["observations"].([]any)
	observations[0].(map[string]any)["credential"] = "forbidden"
	if schema.Validate(object) == nil {
		t.Fatal("open observation schema")
	}
	for _, id := range []RouteID{RoutePRRepairCommand, RoutePRRepairCheck} {
		route, ok := V1Route(id)
		if !ok || route.Budget > MutationBudget {
			t.Fatal("unbounded route", id, route)
		}
	}
}
