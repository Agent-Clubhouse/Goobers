package schemas

import "testing"

func TestInstanceSchemaIsolationMandates(t *testing.T) {
	schema := compileInstanceSchema(t)
	for _, tc := range []struct {
		name, isolation string
		valid           bool
	}{
		{"valid", "mandates: [{match: {stageClass: agentic}, restrictions: ['tmp:ephemeral']}]", true},
		{"empty", "mandates: []", false},
		{"missing selector", "mandates: [{restrictions: ['tmp:ephemeral']}]", false},
		{"wildcard", "mandates: [{match: {stageClass: '*'}, restrictions: ['tmp:ephemeral']}]", false},
		{"unknown field", "mandates: [{match: {stageClass: agentic, gaggle: '*'}, restrictions: ['tmp:ephemeral']}]", false},
		{"unknown effect", "mandates: [{match: {stageClass: agentic}, restrictions: ['tmp:forever']}]", false},
		{"duplicate", "mandates: [{match: {stageClass: agentic}, restrictions: ['tmp:ephemeral', 'tmp:ephemeral']}]", false},
		{"empty effects", "mandates: [{match: {stageClass: agentic}, restrictions: []}]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInstanceYAML(t, schema, "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\nisolation: {"+tc.isolation+"}\n")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
		})
	}
}
