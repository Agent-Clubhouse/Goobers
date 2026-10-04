package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/instance"
)

func TestConfigAuthoringReaderUsesCanonicalWorkflowSource(t *testing.T) {
	base := t.TempDir()
	layout := instance.NewLayout(filepath.Join(base, "instance"))
	external := filepath.Join(base, "authored")
	writeConfigAuthoringTestDocument(t, layout.ConfigDir(), "runtime copy\n")
	writeConfigAuthoringTestDocument(t, external, "canonical source\n")

	reader, err := newConfigAuthoringReader(context.Background(), layout, &instance.Config{
		WorkflowSource: &instance.WorkflowSource{
			Kind: instance.WorkflowSourceKindLocalDir,
			Path: external,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	document, err := reader.Document(context.Background(), "source:local", "manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if document.Content != "canonical source\n" || !document.Document.Editable {
		t.Fatalf("local document = content %q, editable %v", document.Content, document.Document.Editable)
	}
}

func TestConfigAuthoringReaderUsesReadOnlyMaterializedGitSnapshot(t *testing.T) {
	base := t.TempDir()
	layout := instance.NewLayout(filepath.Join(base, "instance"))
	external := filepath.Join(base, "repository")
	writeConfigAuthoringTestDocument(t, layout.ConfigDir(), "materialized snapshot\n")
	writeConfigAuthoringTestDocument(t, external, "repository checkout\n")

	reader, err := newConfigAuthoringReader(context.Background(), layout, &instance.Config{
		WorkflowSource: &instance.WorkflowSource{
			Kind: instance.WorkflowSourceKindGit,
			Path: external,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := reader.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources.Items) != 1 ||
		sources.Items[0].Kind != apicontract.ConfigSourceGit ||
		sources.Items[0].Capabilities.DirectWrite {
		t.Fatalf("git source = %+v", sources.Items)
	}
	document, err := reader.Document(context.Background(), "source:local", "manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if document.Content != "materialized snapshot\n" || document.Document.Editable {
		t.Fatalf("git document = content %q, editable %v", document.Content, document.Document.Editable)
	}
}

func writeConfigAuthoringTestDocument(t *testing.T, root, content string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
