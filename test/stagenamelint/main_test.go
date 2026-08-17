package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRepositoryRejectsShippedWorkflowAndLabelLiterals(t *testing.T) {
	root := fixtureRepository(t)
	writeFixture(t, root, "cmd/goobers/stage.go", `package main
func stage(workflow string) bool {
	labels := "goobers:ready,goobers:claimed"
	return workflow == "renamed-workflow" || workflow == "goobers:approved" || labels != ""
}
`)

	violations, err := checkRepositoryWithExceptions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 3 {
		t.Fatalf("violations = %+v, want three", violations)
	}
	messages := violations[0].Message + violations[1].Message
	if !strings.Contains(messages, "workflow role marker") || !strings.Contains(messages, "stage's label input") {
		t.Fatalf("violations do not point to config alternatives: %+v", violations)
	}
}

func TestCheckRepositoryIgnoresTestsAndUnshippedNames(t *testing.T) {
	root := fixtureRepository(t)
	writeFixture(t, root, "cmd/goobers/stage.go", "package main\nconst name = \"custom-workflow\"\n")
	writeFixture(t, root, "internal/helper/helper_test.go", "package helper\nconst name = \"renamed-workflow\"\n")

	violations, err := checkRepositoryWithExceptions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v, want none", violations)
	}
}

func TestExceptionsIgnoreUnrelatedLineShifts(t *testing.T) {
	root := fixtureRepository(t)
	const path = "cmd/goobers/stage.go"
	writeFixture(t, root, path, `package main


const name = "renamed-workflow"
`)

	violations, err := checkRepositoryWithExceptions(root, []exception{{
		Path: path, Value: "renamed-workflow", Reason: "fixture registry",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %+v, want none", violations)
	}
}

func TestExceptionAllowsRepeatedLiteralsWithoutDependingOnCount(t *testing.T) {
	root := fixtureRepository(t)
	const path = "cmd/goobers/stage.go"
	configured := []exception{{
		Path: path, Value: "renamed-workflow", Reason: "fixture registry and lookup",
	}}

	for _, contents := range []string{
		"package main\nconst registry = \"renamed-workflow\"\nconst lookup = \"renamed-workflow\"\n",
		"package main\nconst registry = \"renamed-workflow\"\n",
	} {
		writeFixture(t, root, path, contents)
		violations, err := checkRepositoryWithExceptions(root, configured)
		if err != nil {
			t.Fatal(err)
		}
		if len(violations) != 0 {
			t.Fatalf("violations = %+v, want none for %q", violations, contents)
		}
	}
}

func TestCheckRepositoryRejectsStaleException(t *testing.T) {
	root := fixtureRepository(t)
	const path = "cmd/goobers/stage.go"
	writeFixture(t, root, path, "package main\nconst name = \"custom-workflow\"\n")

	violations, err := checkRepositoryWithExceptions(root, []exception{{
		Path: path, Value: "renamed-workflow", Reason: "fixture registry",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0].Message, "stale exception") {
		t.Fatalf("violations = %+v, want one stale-exception violation", violations)
	}
}

func TestCheckRepositoryRejectsExceptionWhenWorkflowIsNoLongerShipped(t *testing.T) {
	root := fixtureRepository(t)
	const path = "cmd/goobers/stage.go"
	writeFixture(t, root, "reference-workflows/gaggles/goobers/workflows/example.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: replacement-workflow
`)
	writeFixture(t, root, path, "package main\nconst name = \"renamed-workflow\"\n")

	violations, err := checkRepositoryWithExceptions(root, []exception{{
		Path: path, Value: "renamed-workflow", Reason: "fixture registry",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0].Message, "stale exception") {
		t.Fatalf("violations = %+v, want one stale-exception violation", violations)
	}
}

func TestRepositoryCompliesWithStageNameLint(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	violations, err := checkRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("repository stage-name violations: %+v", violations)
	}
}

func fixtureRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, "reference-workflows/gaggles/goobers/workflows/example.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: renamed-workflow
`)
	if err := os.MkdirAll(filepath.Join(root, "cmd", "goobers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
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
