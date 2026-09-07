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
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "goobers"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WalkDefinitionTrees(root, func(string) error { return nil }); err == nil {
		t.Fatal("shared goober file accepted as a directory")
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
