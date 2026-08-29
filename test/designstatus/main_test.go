package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDocumentAcceptsEnumWithMarkdownAndDetail(t *testing.T) {
	t.Parallel()
	path := writeDocument(t, "# Design\n\n> **Status:** Implemented — GA in #1939\n")

	if err := validateDocument(path); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDocumentRejectsMissingMarker(t *testing.T) {
	t.Parallel()
	path := writeDocument(t, "# Design\n\nNo status here.\n")

	err := validateDocument(path)
	if err == nil || !strings.Contains(err.Error(), "missing Status marker") {
		t.Fatalf("error = %v, want missing marker", err)
	}
}

func TestValidateDocumentRejectsUnknownStatus(t *testing.T) {
	t.Parallel()
	path := writeDocument(t, "# Design\n\nStatus: proposed\n")

	err := validateDocument(path)
	if err == nil || !strings.Contains(err.Error(), `unknown status "proposed"`) {
		t.Fatalf("error = %v, want unknown status", err)
	}
}

func TestValidateTreesChecksNestedMarkdownDocuments(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentAt(t, filepath.Join(root, "valid.md"), "# Design\n\n- Status: approved\n")
	writeDocumentAt(t, filepath.Join(root, "nested", "invalid.md"), "# Design\n\nStatus: exploratory\n")
	writeDocumentAt(t, filepath.Join(root, "ignored.txt"), "Status: exploratory\n")

	err := validateTrees(root)
	if err == nil || !strings.Contains(err.Error(), "nested") || !strings.Contains(err.Error(), "exploratory") {
		t.Fatalf("error = %v, want nested invalid document", err)
	}
}

func writeDocument(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "document.md")
	writeDocumentAt(t, path, content)
	return path
}

func writeDocumentAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
