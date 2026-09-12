package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbeddedSQLiteStoresDeclareSchemaVersioning discovers production
// packages that open SQLite directly and requires their package to own schema
// metadata or call the shared migrator. This makes adding an unversioned store
// fail the ordinary CI suite instead of relying on another repository audit.
func TestEmbeddedSQLiteStoresDeclareSchemaVersioning(t *testing.T) {
	root := moduleRoot(t)
	internal := filepath.Join(root, "internal")
	storeDirs := map[string]bool{}
	packageSource := map[string]string{}
	err := filepath.WalkDir(internal, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dir := filepath.Dir(path)
		text := string(content)
		packageSource[dir] += text
		if strings.Contains(text, `sql.Open("sqlite"`) {
			storeDirs[dir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(storeDirs) == 0 {
		t.Fatal("no embedded SQLite stores discovered; update the detector")
	}
	for dir := range storeDirs {
		source := packageSource[dir]
		if !strings.Contains(source, "schema_meta") &&
			!strings.Contains(source, "schema_version") &&
			!strings.Contains(source, "sqliteschema.Migrate") {
			rel, _ := filepath.Rel(root, dir)
			t.Errorf("embedded SQLite store package %s has no versioned schema metadata", filepath.ToSlash(rel))
		}
	}
}
