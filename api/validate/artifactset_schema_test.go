package validate

import "testing"

func TestArtifactSetSchemasRejectInvalidWireShapes(t *testing.T) {
	v := newV(t)
	for _, test := range []struct{ schema, data string }{
		{"stage-artifact-manifest.schema.json", `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":null}`},
		{"stage-artifact-manifest.schema.json", `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[{"name":"report","path":"../secret","mediaType":"text/plain"}]}`},
		{"stage-artifact-manifest.schema.json", `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[{"name":"report","path":"report","mediaType":"text/plain","digest":"self-authored"}]}`},
		{"stage-artifact-set.schema.json", `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[{"name":"report","slot":0,"artifact":{"path":"artifacts/report","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}]}`},
		{"stage-artifact-set.schema.json", `{"schemaVersion":"future","entries":[]}`},
	} {
		if err := v.ValidateJSON(test.schema, []byte(test.data)); err == nil {
			t.Fatalf("%s accepted invalid wire shape: %s", test.schema, test.data)
		}
	}
	for _, schema := range []string{"stage-artifact-manifest.schema.json", "stage-artifact-set.schema.json"} {
		if err := v.ValidateJSON(schema, []byte(`{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[]}`)); err != nil {
			t.Fatalf("%s refused empty set: %v", schema, err)
		}
	}
}
