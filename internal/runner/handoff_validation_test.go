package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/handoffcheck"
)

const runnerHandoffSchema = `{
  "type": "object",
  "required": ["verdict"],
  "additionalProperties": false,
  "properties": {
    "verdict": {"enum": ["pass", "fail"]}
  }
}`

func publishHandoffSet(t *testing.T, root, payload string) []apiv1.ContextPointer {
	t.Helper()
	record := func(name, mediaType string, data []byte) (apiv1.ArtifactPointer, error) {
		return apiv1.WriteArtifact(root, filepath.Join("artifacts", "produce", name), data, mediaType)
	}
	prepared, err := artifactset.Prepare(context.Background(), writeManifestWorkspace(t, payload), "manifest.json", func(_ string, data []byte) ([]byte, error) {
		return data, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Bind(&apiv1.ArtifactPublication{
		Stage: "produce", Visit: 1, Slots: []apiv1.ArtifactSlot{{Name: "report", MediaType: "application/json"}},
	}, 1); err != nil {
		t.Fatal(err)
	}
	pointers, err := prepared.Publish(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	return contextPointersFor("produce", pointers)
}

func writeManifestWorkspace(t *testing.T, payload string) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "payload.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "manifest.json"), []byte(`{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[{"name":"report","path":"payload.json","mediaType":"application/json"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func testHandoffSchemaLoader(t *testing.T) HandoffSchemaLoader {
	t.Helper()
	return func(schemaPath string) (*handoffcheck.Schema, error) {
		return handoffcheck.Compile(schemaPath, "", []byte(runnerHandoffSchema))
	}
}

func TestBuildHandoffValidationReportValid(t *testing.T) {
	root := t.TempDir()
	report := buildHandoffValidationReport(context.Background(), root, map[string]handoffBinding{
		"summary": {LocalName: "summary", ProducerTask: "produce", SlotName: "report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"},
	}, publishHandoffSet(t, root, `{"verdict":"pass"}`), testHandoffSchemaLoader(t))
	if report == nil {
		t.Fatal("want report")
	}
	if report.InputValid != handoffcheck.InputValidTrue || report.Error != "" || len(report.Entries) != 1 || !report.Entries[0].Valid {
		t.Fatalf("%+v", report)
	}
	if report.Entries[0].SchemaID != "schemas/report.schema.json" {
		t.Fatalf("schema id = %q", report.Entries[0].SchemaID)
	}
}

func TestBuildHandoffValidationReportInvalid(t *testing.T) {
	root := t.TempDir()
	report := buildHandoffValidationReport(context.Background(), root, map[string]handoffBinding{
		"summary": {LocalName: "summary", ProducerTask: "produce", SlotName: "report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"},
	}, publishHandoffSet(t, root, `{"verdict":"maybe"}`), testHandoffSchemaLoader(t))
	if report == nil {
		t.Fatal("want report")
	}
	if report.InputValid != handoffcheck.InputValidFalse || len(report.Entries) != 1 || report.Entries[0].Valid {
		t.Fatalf("%+v", report)
	}
	if len(report.Entries[0].Issues) == 0 || report.Entries[0].Issues[0].Code != handoffcheck.CodeSchemaViolation {
		t.Fatalf("issues = %+v", report.Entries[0].Issues)
	}
}

func TestBuildHandoffValidationReportUnknownOnLoaderError(t *testing.T) {
	root := t.TempDir()
	report := buildHandoffValidationReport(context.Background(), root, map[string]handoffBinding{
		"summary": {LocalName: "summary", ProducerTask: "produce", SlotName: "report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"},
	}, publishHandoffSet(t, root, `{"verdict":"pass"}`), func(string) (*handoffcheck.Schema, error) {
		return nil, os.ErrNotExist
	})
	if report == nil {
		t.Fatal("want report")
	}
	if report.InputValid != handoffcheck.InputValidUnknown || !strings.Contains(report.Error, "load schema") {
		t.Fatalf("%+v", report)
	}
}
