package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRepositoryRejectsAnonymousFlakeSkipAndWorkflowRetry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/example_test.go", `package pkg
import "testing"
func TestExample(t *testing.T) { t.Skip("flaky under load") }
`)
	writeFixture(t, root, ".github/workflows/ci.yml", `name: CI
jobs:
  test:
    steps:
      - uses: nick-fields/retry@v3
`)
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 2 {
		t.Fatalf("violations = %+v, want two", violations)
	}
	if !strings.Contains(violations[0].Message+violations[1].Message, "flake.Quarantine") ||
		!strings.Contains(violations[0].Message+violations[1].Message, "workflow retries") {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestCheckWorkflowRejectsCommonRetryForms(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"retries: 3",
		"max_attempts: 3",
		"uses: Wandalen/wretry.action@v3",
	} {
		root := t.TempDir()
		writeFixture(t, root, ".github/workflows/ci.yml", "jobs:\n  test:\n    "+line+"\n")
		violations, err := checkRepository(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(violations) != 1 {
			t.Fatalf("line %q violations = %+v, want one", line, violations)
		}
	}
}

func TestCheckRepositoryAllowsNonFlakeSkipAndQuarantineHelper(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/example_test.go", `package pkg
import "testing"
func TestExample(t *testing.T) { t.Skip("requires Linux") }
`)
	writeFixture(t, root, quarantineHelper, `package flake
func quarantine(t interface{ Skip(string) }) { t.Skip("quarantined flaky test") }
`)
	writeFixture(t, root, ".github/workflows/ci.yml", `name: CI
jobs:
  test:
    steps:
      - run: go test ./...
`)
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v", violations)
	}
}

func TestRepositoryCompliesWithFlakePolicy(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("repository flake-policy violations: %+v", violations)
	}
}

func writeFixture(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
