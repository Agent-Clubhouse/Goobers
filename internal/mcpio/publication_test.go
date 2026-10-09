package mcpio

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/handoffcheck"
)

const findingSchema = `{
	"type": "object",
	"required": ["summary", "severity"],
	"properties": {
		"summary": {"type": "string"},
		"severity": {"type": "integer"}
	}
}`

const findingManifest = `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[{"name":"finding","path":"out/finding.json","mediaType":"application/json"}]}`

func schemaToolset(t *testing.T) (*Toolset, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	return NewToolset(Config{
		Workspace:            root,
		ArtifactManifestFile: "output/manifest.json",
		PublicationSchemas:   []PublicationSchema{{Slot: "finding", SchemaID: "schemas/finding.schema.json", Document: json.RawMessage(findingSchema)}},
	}), root
}

func writePayload(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "out", "finding.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireRejection(t *testing.T, err error, category string) RejectedOutput {
	t.Helper()
	rejection, ok := asPublicationRejection(err)
	if !ok {
		t.Fatalf("want PublicationRejection, got %v", err)
	}
	if rejection.Status != "rejected" || rejection.Instructions != PublicationRepairInstructions || len(rejection.Outputs) != 1 {
		t.Fatalf("rejection=%+v", rejection)
	}
	out := rejection.Outputs[0]
	if out.Slot != "finding" || out.SchemaID != "schemas/finding.schema.json" || out.Category != category || len(out.Issues) == 0 {
		t.Fatalf("output=%+v", out)
	}
	var decoded PublicationRejection
	if err := json.Unmarshal([]byte(err.Error()), &decoded); err != nil {
		t.Fatalf("tool error text is not structured JSON: %v", err)
	}
	return out
}

func TestPublishOutputRejectsSchemaBoundOutputs(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, payload, category, code, path string
	}{
		{name: "invalid JSON", manifest: findingManifest, payload: `{"summary": "x",`, category: RejectSchemaViolation, code: handoffcheck.CodeTruncated},
		{name: "missing required field", manifest: findingManifest, payload: `{"severity": 1}`, category: RejectSchemaViolation, code: handoffcheck.CodeSchemaViolation},
		{name: "wrong type", manifest: findingManifest, payload: `{"summary": "x", "severity": "high"}`, category: RejectSchemaViolation, code: handoffcheck.CodeSchemaViolation, path: "/severity"},
		{name: "omitted output", manifest: `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[]}`, category: RejectMissingOutput, code: RejectMissingOutput},
		{name: "unreadable payload", manifest: strings.Replace(findingManifest, "out/finding.json", "out/absent.json", 1), category: RejectUnreadableOutput, code: RejectUnreadableOutput},
		{name: "malformed manifest", manifest: `{"entries":`, category: RejectInvalidManifest, code: handoffcheck.CodeTruncated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, root := schemaToolset(t)
			if tc.payload != "" {
				writePayload(t, root, tc.payload)
			}
			_, _, err := tool.PublishOutput(tc.manifest)
			out := requireRejection(t, err, tc.category)
			if out.Issues[0].Code != tc.code || (tc.path != "" && out.Issues[0].Path != tc.path) {
				t.Fatalf("issues=%+v", out.Issues)
			}
			if _, statErr := os.Stat(filepath.Join(root, "output", "manifest.json")); !os.IsNotExist(statErr) {
				t.Fatalf("rejected publication wrote the manifest: %v", statErr)
			}
		})
	}
}

func TestPublishOutputMissingFieldNamesTheField(t *testing.T) {
	tool, root := schemaToolset(t)
	writePayload(t, root, `{"severity": 1}`)
	_, _, err := tool.PublishOutput(findingManifest)
	if !strings.Contains(err.Error(), "summary") {
		t.Fatalf("rejection does not name the missing field: %v", err)
	}
}

func TestPublishOutputRejectionDoesNotEchoPayloadValues(t *testing.T) {
	tool, root := schemaToolset(t)
	writePayload(t, root, `{"summary": 7, "severity": "hunter2-secret-value"}`)
	_, _, err := tool.PublishOutput(findingManifest)
	requireRejection(t, err, RejectSchemaViolation)
	if strings.Contains(err.Error(), "hunter2-secret-value") {
		t.Fatalf("rejection echoed a payload value: %v", err)
	}
}

func TestPublishOutputRepublishAfterRejectionReplacesAtomically(t *testing.T) {
	tool, root := schemaToolset(t)
	writePayload(t, root, `{"summary": "first", "severity": 1}`)
	if _, _, err := tool.PublishOutput(findingManifest); err != nil {
		t.Fatalf("valid publication rejected: %v", err)
	}
	manifestPath := filepath.Join(root, "output", "manifest.json")

	writePayload(t, root, `{"summary": "second"}`)
	const replacement = `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[{"name":"finding","path":"out/finding.json","mediaType":"application/json"}],"note":1}`
	_, _, err := tool.PublishOutput(replacement)
	requireRejection(t, err, RejectSchemaViolation)
	if data, readErr := os.ReadFile(manifestPath); readErr != nil || string(data) != findingManifest {
		t.Fatalf("rejected republish disturbed the accepted manifest: %q, %v", data, readErr)
	}

	writePayload(t, root, `{"summary": "second", "severity": 2}`)
	if _, _, err := tool.PublishOutput(replacement); err != nil {
		t.Fatalf("corrected publication rejected: %v", err)
	}
	if data, readErr := os.ReadFile(manifestPath); readErr != nil || string(data) != replacement {
		t.Fatalf("corrected publication not committed: %q, %v", data, readErr)
	}
}

func TestPublishOutputWithoutSchemasKeepsExistingBehavior(t *testing.T) {
	root := t.TempDir()
	tool := NewToolset(Config{Workspace: root, ArtifactManifestFile: "manifest.json"})
	if _, _, err := tool.PublishOutput(`not even json`); err != nil {
		t.Fatalf("unconstrained manifest publication changed behavior: %v", err)
	}
}

func TestLoadConfigRejectsUncompilablePublicationSchema(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Workspace: root, PublicationSchemas: []PublicationSchema{{Slot: "finding", SchemaID: "bad", Document: json.RawMessage(`{"type": 7}`)}}}
	path, err := WriteJSON(root, "config.json", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "finding") {
		t.Fatalf("invalid publication schema accepted: %v", err)
	}
}

func asPublicationRejection(err error) (*PublicationRejection, bool) {
	var rejection *PublicationRejection
	return rejection, errors.As(err, &rejection)
}
