package configauthoring

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func TestLocalReaderListsAndReadsSafeDocuments(t *testing.T) {
	root := testSource(t)
	reader, err := NewReader(root, apicontract.ConfigSourceLocal, true)
	if err != nil {
		t.Fatal(err)
	}

	sources, err := reader.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources.Items) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources.Items))
	}
	source := sources.Items[0]
	if source.ID != localSourceID || source.Revision == "" {
		t.Fatalf("source = %+v", source)
	}
	if !source.Capabilities.Read || !source.Capabilities.Validate || !source.Capabilities.DirectWrite {
		t.Fatalf("capabilities = %+v", source.Capabilities)
	}

	page, err := reader.Documents(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 5 {
		t.Fatalf("documents = %+v, want 5 safe authorable files", page.Items)
	}
	gaggle := findDocument(t, page.Items, "gaggles/core/gaggle.yaml")
	if gaggle.Definition == nil || gaggle.Definition.Kind != apicontract.ConfigDocumentGaggle ||
		gaggle.Definition.Name != "core" {
		t.Fatalf("gaggle definition = %+v", gaggle.Definition)
	}
	workflow := findDocument(t, page.Items, "gaggles/core/workflows/implementation.yaml")
	if workflow.Definition == nil || workflow.Definition.Name != "implementation" ||
		workflow.Definition.Gaggle != "core" {
		t.Fatalf("workflow definition = %+v", workflow.Definition)
	}
	sharedGoober := findDocument(t, page.Items, "goobers/reviewer.yaml")
	if sharedGoober.Definition == nil ||
		sharedGoober.Definition.Name != "reviewer" {
		t.Fatalf("shared goober = %+v", sharedGoober)
	}
	support := findDocument(t, page.Items, "gaggles/core/goobers/implementer/instructions.md")
	if support.Definition == nil || support.Definition.Kind != apicontract.ConfigDocumentSupport {
		t.Fatalf("support definition = %+v", support.Definition)
	}

	document, err := reader.Document(context.Background(), source.ID, workflow.Path)
	if err != nil {
		t.Fatal(err)
	}
	if document.Revision != page.Revision || document.Document.ETag == "" || document.Content == "" {
		t.Fatalf("document = %+v", document)
	}
	if len(document.Diagnostics) == 0 {
		t.Fatal("expected workflow validation metadata")
	}
}

func TestLocalReaderRevisionAndETagFollowContent(t *testing.T) {
	root := testSource(t)
	reader, err := NewReader(root, apicontract.ConfigSourceLocal, true)
	if err != nil {
		t.Fatal(err)
	}
	path := "gaggles/core/workflows/implementation.yaml"
	before, err := reader.Document(context.Background(), localSourceID, path)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, filepath.FromSlash(path))
	if err := os.WriteFile(file, []byte(workflowYAML+"# changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := reader.Document(context.Background(), localSourceID, path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision == after.Revision {
		t.Fatal("source revision did not change")
	}
	if before.Document.ETag == after.Document.ETag {
		t.Fatal("document ETag did not change")
	}
}

func TestLocalReaderRejectsUnsafeDocuments(t *testing.T) {
	root := testSource(t)
	reader, err := NewReader(root, apicontract.ConfigSourceLocal, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, logicalPath := range []string{
		"../manifest.yaml",
		`gaggles\core\gaggle.yaml`,
		".env",
		"credentials.yaml",
		"notes.txt",
	} {
		t.Run(logicalPath, func(t *testing.T) {
			_, err := reader.Document(context.Background(), localSourceID, logicalPath)
			if !errors.Is(err, ErrDocumentNotFound) {
				t.Fatalf("Document() error = %v, want ErrDocumentNotFound", err)
			}
		})
	}

	target := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := reader.Document(context.Background(), localSourceID, "linked.yaml"); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("symlink Document() error = %v, want ErrDocumentNotFound", err)
	}
}

func TestLocalReaderRejectsSensitiveDirectories(t *testing.T) {
	root := testSource(t)
	sensitivePaths := []string{
		"credentials/service.yaml",
		"secrets/readme.md",
	}
	for _, logicalPath := range sensitivePaths {
		writeTestFile(t, root, logicalPath, "sensitive\n")
	}
	reader, err := NewReader(root, apicontract.ConfigSourceLocal, true)
	if err != nil {
		t.Fatal(err)
	}

	page, err := reader.Documents(context.Background(), localSourceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, logicalPath := range sensitivePaths {
		t.Run(logicalPath, func(t *testing.T) {
			for _, document := range page.Items {
				if document.Path == logicalPath {
					t.Fatalf("Documents() exposed sensitive path %q", logicalPath)
				}
			}
			_, err := reader.Document(context.Background(), localSourceID, logicalPath)
			if !errors.Is(err, ErrDocumentNotFound) {
				t.Fatalf("Document() error = %v, want ErrDocumentNotFound", err)
			}
		})
	}
}

func TestLocalReaderAdvertisesReadOnlySource(t *testing.T) {
	reader, err := NewReader(testSource(t), apicontract.ConfigSourceLocal, false)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := reader.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sources.Items[0].Capabilities.DirectWrite {
		t.Fatalf("read-only capabilities = %+v", sources.Items[0].Capabilities)
	}
	page, err := reader.Documents(context.Background(), localSourceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range page.Items {
		if document.Editable {
			t.Fatalf("read-only document is editable: %+v", document)
		}
	}
}

func TestReaderAdvertisesResolvedGitSourceAsReadOnly(t *testing.T) {
	reader, err := NewReader(testSource(t), apicontract.ConfigSourceGit, false)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := reader.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source := sources.Items[0]
	if source.Kind != apicontract.ConfigSourceGit || source.Capabilities.DirectWrite {
		t.Fatalf("git source = %+v", source)
	}
}

func testSource(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "config")
	writeTestFile(t, root, "manifest.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: test
spec:
  instance:
    name: test
  gaggles:
    - core
`)
	writeTestFile(t, root, "gaggles/core/gaggle.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: core
spec: {}
`)
	writeTestFile(t, root, "gaggles/core/workflows/implementation.yaml", workflowYAML)
	writeTestFile(t, root, "gaggles/core/goobers/implementer/instructions.md", "# Implementer\n")
	writeTestFile(t, root, ".env", "SECRET=value\n")
	writeTestFile(t, root, "credentials.yaml", "token: nope\n")
	writeTestFile(t, root, "notes.txt", "not authorable\n")
	writeTestFile(t, filepath.Dir(root), "goobers/reviewer.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: reviewer
spec:
  gaggle: core
`)
	return root
}

const workflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: implementation
spec:
  gaggle: core
`

func writeTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func findDocument(t *testing.T, documents []apicontract.ConfigDocumentDescriptor, logicalPath string) apicontract.ConfigDocumentDescriptor {
	t.Helper()
	for _, document := range documents {
		if document.Path == logicalPath {
			return document
		}
	}
	t.Fatalf("document %q not found in %+v", logicalPath, documents)
	return apicontract.ConfigDocumentDescriptor{}
}
