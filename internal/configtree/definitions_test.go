package configtree

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWalkDefinitionTrees(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config")
	shared := filepath.Join(filepath.Dir(root), "goobers")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var visited []string
	walk := func(path string) error { visited = append(visited, path); return nil }
	if err := WalkDefinitionTrees(root, walk); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(visited, []string{root}) {
		t.Fatalf("absent shared root: %v", visited)
	}
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	visited = nil
	if err := WalkDefinitionTrees(root, walk); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(visited, []string{root, shared}) {
		t.Fatalf("walk order: %v", visited)
	}
	want := errors.New("cannot read tree")
	for _, failure := range []string{root, shared} {
		if err := WalkDefinitionTrees(root, func(path string) error {
			if path == failure {
				return want
			}
			return nil
		}); !errors.Is(err, want) {
			t.Fatalf("error for %s: %v", failure, err)
		}
	}
}

func TestWalkDefinitionTreesRejectsSharedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "goobers"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WalkDefinitionTrees(root, func(string) error { return nil }); err == nil {
		t.Fatal("shared goober file accepted as a directory")
	}
}

func TestWalkDefinitionTreesRejectsInvalidPrimaryRootBeforeCallback(t *testing.T) {
	parent := t.TempDir()
	cases := map[string]func(string) string{
		"missing": func(parent string) string {
			return filepath.Join(parent, "missing")
		},
		"file": func(parent string) string {
			root := filepath.Join(parent, "config-file")
			if err := os.WriteFile(root, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return root
		},
	}
	for name, root := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			err := WalkDefinitionTrees(root(parent), func(string) error {
				called = true
				return nil
			})
			if err == nil {
				t.Fatal("invalid primary root accepted")
			}
			if called {
				t.Fatal("walk callback invoked for invalid primary root")
			}
		})
	}
}

func TestWalkDefinitionTreesResolvesSiblingFromRelativeRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "config")
	shared := filepath.Join(parent, "goobers")
	for _, path := range []string{root, shared} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	var visited []string
	if err := WalkDefinitionTrees(".", func(path string) error {
		absolute, err := filepath.Abs(path)
		visited = append(visited, absolute)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(visited, []string{root, shared}) {
		t.Fatalf("relative root visited %v", visited)
	}
}

func TestWalkDefinitionTreesIgnoresUnrelatedSibling(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "cfg")
	shared := filepath.Join(parent, "goobers")
	for _, path := range []string{root, shared} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var visited []string
	if err := WalkDefinitionTrees(root, func(path string) error {
		visited = append(visited, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(visited, []string{root}) {
		t.Fatalf("unrecognized config layout visited %v", visited)
	}
}

func TestWalkDefinitionTreesKeepsSiblingPathsRelativeToConfigRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "config")
	shared := filepath.Join(parent, "goobers")
	for _, path := range []string{root, shared} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	var visited []string
	if err := WalkDefinitionTrees(".", func(path string) error {
		visited = append(visited, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(visited, []string{".", filepath.Join("..", "goobers")}) {
		t.Fatalf("relative sibling paths = %v", visited)
	}
}

func TestSharedGooberDirectoryWithoutManifest(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "config")
	flat := filepath.Join(parent, "cfg")
	for _, dir := range []string{nested, flat, filepath.Join(parent, "goobers")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := SharedGooberDirectory(nested); got == "" {
		t.Fatal("manifestless nested config root must expose its sibling goobers tree")
	}
	if got := SharedGooberDirectory(flat); got != "" {
		t.Fatalf("flat root must ignore unrelated sibling goobers, got %q", got)
	}
	var visited []string
	if err := WalkDefinitionTrees(flat, func(p string) error { visited = append(visited, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(visited, []string{flat}) {
		t.Fatalf("flat walk: %v", visited)
	}
}
