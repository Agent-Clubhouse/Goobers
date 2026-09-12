package main

import (
	"path/filepath"
	"testing"
)

// Keep real repository declarations under the ordinary unit gate. Fixture-only
// scanner tests cannot catch an integration test that uses an unrecognized
// dependency helper or hides its required first-statement environment guard.
func TestRepositoryIntegrationPolicy(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	result, err := scanIntegration(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.packages) == 0 {
		t.Fatal("repository has no integration test packages")
	}
	if err := validateInventory(result.dependencies); err != nil {
		t.Fatal(err)
	}
}
