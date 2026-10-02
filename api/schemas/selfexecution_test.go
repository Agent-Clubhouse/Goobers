package schemas

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelfExecutionSchemaAndHostedReference(t *testing.T) {
	schema := compileInstanceSchema(t)
	for _, policy := range []string{"allow", "deny", "invalid"} {
		err := validateInstanceYAML(t, schema, "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\nplacement:\n  selfExecution: "+policy+"\n")
		if (err != nil) != (policy == "invalid") {
			t.Fatalf("%s: %v", policy, err)
		}
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "reference", "instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstanceYAML(t, schema, string(data)); err != nil {
		t.Fatal(err)
	}
}
