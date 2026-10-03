package configtree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/yamldoc"
)

func TestWalkRawYAMLDocs(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "config")
	shared := filepath.Join(parent, "goobers")

	writeDocumentFile(t, filepath.Join(root, "definitions.YML"), `kind: Gaggle
metadata: {name: first}
---
kind: Workflow
metadata: {name: second}
`)
	writeDocumentFile(t, filepath.Join(root, "ignored.txt"), "kind: Goober\nmetadata: {name: text}\n")
	writeDocumentFile(t, filepath.Join(root, ".hidden", "hidden.yaml"), "kind: Goober\nmetadata: {name: hidden}\n")
	writeDocumentFile(t, filepath.Join(root, "gaggles", "team", "skills", "skill.yaml"), "kind: Goober\nmetadata: {name: skill}\n")
	writeDocumentFile(t, filepath.Join(root, "goobers", "local", "assets", "asset.yaml"), "kind: Goober\nmetadata: {name: asset}\n")
	writeDocumentFile(t, filepath.Join(shared, "shared.yaml"), "kind: Goober\nmetadata: {name: shared}\n")

	type visitedDoc struct {
		path string
		rel  string
		kind string
		name string
	}
	var visited []visitedDoc
	err := WalkRawYAMLDocs(root, func(path, rel string, doc yamldoc.ParsedDoc) error {
		visited = append(visited, visitedDoc{
			path: path,
			rel:  rel,
			kind: doc.Meta.Kind,
			name: doc.Meta.Name,
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []visitedDoc{
		{path: filepath.Join(root, "definitions.YML"), rel: "definitions.YML", kind: "Gaggle", name: "first"},
		{path: filepath.Join(root, "definitions.YML"), rel: "definitions.YML", kind: "Workflow", name: "second"},
		{
			path: filepath.Join(shared, "shared.yaml"),
			rel:  filepath.Join("..", "goobers", "shared.yaml"),
			kind: "Goober",
			name: "shared",
		},
	}
	if !reflect.DeepEqual(visited, want) {
		t.Fatalf("visited documents = %#v, want %#v", visited, want)
	}
}

func TestWalkRawYAMLDocsWrapsErrors(t *testing.T) {
	root := t.TempDir()
	writeDocumentFile(t, filepath.Join(root, "definition.yaml"), "kind: Gaggle\nmetadata: {name: example}\n")

	sentinel := errors.New("stop")
	err := WalkRawYAMLDocs(root, func(string, string, yamldoc.ParsedDoc) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WalkRawYAMLDocs error = %v, want wrapped sentinel", err)
	}
	if want := fmt.Sprintf("walk %s: stop", root); err.Error() != want {
		t.Fatalf("WalkRawYAMLDocs error = %q, want %q", err, want)
	}

	missing := filepath.Join(t.TempDir(), "missing")
	err = WalkRawYAMLDocs(missing, func(string, string, yamldoc.ParsedDoc) error {
		return nil
	})
	if prefix := fmt.Sprintf("walk %s: ", missing); err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("WalkRawYAMLDocs missing root error = %v, want prefix %q", err, prefix)
	}
}

func writeDocumentFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
