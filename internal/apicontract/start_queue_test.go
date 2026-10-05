package apicontract

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestStartQueueClosedWireContract(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"components": map[string]any{"schemas": openAPIStartQueueSchemas()}})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newWireFixtures()
	for name, value := range map[string]any{"StartQueuePage": fixture.StartQueue, "StartQueueItem": fixture.StartQueueItem, "StartQueueCancelInput": fixture.StartQueueCancel} {
		compiler := jsonschema.NewCompiler()
		if err = compiler.AddResource("queue.json", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile("queue.json#/components/schemas/" + name)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err = json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if err = schema.Validate(decoded); err != nil {
			t.Fatal(name, err)
		}
		decoded["authority"] = map[string]any{"role": "admin"}
		if err = schema.Validate(decoded); err == nil {
			t.Fatal("accepted authority override", name)
		}
	}
}
