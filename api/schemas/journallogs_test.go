package schemas

import "testing"

func TestInstanceSchemaJournalLogs(t *testing.T) {
	schema := compileInstanceSchema(t)
	for _, value := range []string{"true", "false", `"false"`} {
		document := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\ntelemetry:\n  otlp:\n    endpoint: localhost:4317\n    journalLogs: " + value + "\n"
		err := validateInstanceYAML(t, schema, document)
		if (err == nil) != (value != `"false"`) {
			t.Fatalf("journalLogs %s: %v", value, err)
		}
	}
}
