package schemas

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestAgentToolkitSchemaAcceptsSupportHistoryCorrection(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	raw, err := FS.ReadFile(AgentToolkitManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource(BaseURI+AgentToolkitManifest, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(BaseURI + AgentToolkitManifest + "#/$defs/dslVersion")
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range supportmatrix.GetDSL().Versions() {
		raw, err := json.Marshal(version)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatalf("DSL %s current metadata: %v", version.Version, err)
		}
		// Older manifest rows omit the additive field and remain schema-valid.
		delete(value, "effectiveIn")
		if err := schema.Validate(value); err != nil {
			t.Fatalf("DSL %s legacy metadata: %v", version.Version, err)
		}
		for _, invalid := range []any{nil, 4, "", "dev", "v0.4.0-beta.1"} {
			value["effectiveIn"] = invalid
			if err := schema.Validate(value); err == nil {
				t.Errorf("DSL %s accepted invalid effectiveIn %#v", version.Version, invalid)
			}
		}
	}
}
