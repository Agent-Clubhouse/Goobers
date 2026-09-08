package schemas

import (
	"bytes"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestCostReportingSchemaStrictNullable(t *testing.T) {
	for _, location := range []struct{ file, pointer string }{
		{"instance.schema.json", "#/properties/cost"},
		{"gaggle.schema.json", "#/properties/spec/properties/cost"},
	} {
		t.Run(location.file, func(t *testing.T) {
			compiler := jsonschema.NewCompiler()
			for _, file := range Files() {
				raw, err := FS.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if err := compiler.AddResource(BaseURI+file, bytes.NewReader(raw)); err != nil {
					t.Fatal(err)
				}
			}
			schema, err := compiler.Compile(BaseURI + location.file + location.pointer)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name  string
				value any
				valid bool
			}{
				{"empty", map[string]any{}, true},
				{"null enabled", map[string]any{"enabled": nil}, true},
				{"true", map[string]any{"enabled": true}, true},
				{"false", map[string]any{"enabled": false}, true},
				{"string", map[string]any{"enabled": "false"}, false},
				{"number", map[string]any{"enabled": float64(0)}, false},
				{"unknown", map[string]any{"enable": false}, false},
				{"extra", map[string]any{"enabled": false, "other": true}, false},
				{"scalar", false, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := schema.Validate(tc.value); (err == nil) != tc.valid {
						t.Fatalf("validation = %v, want valid=%v", err, tc.valid)
					}
				})
			}
		})
	}
}
