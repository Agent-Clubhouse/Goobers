package validate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestGaggleHealthResponseSchemaMatchesGoType(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	response := apiv1.GaggleHealthResponse{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		Health: apiv1.GaggleHealthSnapshot{
			SchemaVersion: apiv1.GaggleHealthSchemaVersion,
			Gaggle:        "example",
			State:         apiv1.GaggleHealthHealthy,
			UpdatedAt:     now,
			Active:        []apiv1.GaggleHealthFinding{},
			History:       []apiv1.GaggleHealthFinding{},
		},
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := newV(t).ValidateJSON(schemas.GaggleHealth, data); err != nil {
		t.Fatalf("health response does not validate: %v", err)
	}
}

func TestGaggleHealthSchemaBoundsSensitiveText(t *testing.T) {
	document := []byte(`{
		"schemaVersion":"goobers.dev/gaggle-health/v1alpha1",
		"health":{
			"schemaVersion":"goobers.dev/gaggle-health/v1alpha1",
			"gaggle":"example",
			"state":"healthy",
			"updatedAt":"2026-10-02T12:00:00Z",
			"active":[],
			"history":[],
			"lastSequence":0,
			"rawLogs":"forbidden"
		}
	}`)
	if err := newV(t).ValidateJSON(schemas.GaggleHealth, document); err == nil {
		t.Fatal("health schema accepted an unrestricted rawLogs field")
	}
}

func TestGaggleSchemaHealthPolicyIsOptionalAndClosed(t *testing.T) {
	base := `{
		"apiVersion":"goobers.dev/v1alpha1",
		"kind":"Gaggle",
		"metadata":{"name":"example"},
		"spec":{
			"project":{"provider":"github","owner":"acme","name":"example"},
			"backlog":{"provider":"github","project":"acme/example"},
			"isolation":{"namespace":"example"}%s
		}
	}`
	for _, tc := range []struct {
		name    string
		health  string
		wantErr bool
	}{
		{name: "existing config unchanged"},
		{name: "complete policy", health: `,"health":{"enabled":true,"evaluationInterval":"5m","thresholds":{"noProgress":"30m","flappingCount":3},"findings":{"orphaned-claim":{"mode":"repair"}},"eventWorkflow":{"enabled":true,"workflow":"health-events","eventTypes":["finding-opened"]}}`},
		{name: "unknown finding rejected", health: `,"health":{"findings":{"invented":{"mode":"observe"}}}`, wantErr: true},
		{name: "workflow cannot add authority", health: `,"health":{"eventWorkflow":{"enabled":true,"workflow":"health-events","authorizeRepair":true}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := []byte(fmt.Sprintf(base, tc.health))
			err := newV(t).ValidateJSON("gaggle.schema.json", document)
			if tc.wantErr && err == nil {
				t.Fatal("expected schema validation failure")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("schema validation failed: %v", err)
			}
		})
	}
}

func TestGaggleHealthHardSafetyPolicyRunsAtConfigLoad(t *testing.T) {
	document := runControlsConfig("  health:\n    findings:\n      no-progress:\n        mode: repair\n", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "health.yaml"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	issues := joinIssues(report)
	if !strings.Contains(issues, "CFG014") || !strings.Contains(issues, "hard safety policy") {
		t.Fatalf("expected hard-safety diagnostic, got:\n%s", issues)
	}
}
