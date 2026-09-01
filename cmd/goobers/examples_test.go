package main

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	configexamples "github.com/goobers/goobers/config-examples"
)

func TestExamplesListFromOutsideCheckout(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	code, stdout, stderr := runArgs(t, "examples", "list")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	const want = "backlog-assignment  Backlog assignment\n" +
		"backlog-curation    Backlog curation\n" +
		"implementation      Implementation (issue -> PR, reviewer gate, CI-poll repass)\n" +
		"work-nomination     Work nomination\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

// TestExamplesListDescriptionsMatchEmbeddedYAML pins each listed description to
// the embedded workflow's spec.displayName so the list cannot drift from the
// YAML it describes.
func TestExamplesListDescriptionsMatchEmbeddedYAML(t *testing.T) {
	examples, err := configexamples.WorkflowExamples()
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "examples", "list")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != len(examples) {
		t.Fatalf("got %d lines, want %d", len(lines), len(examples))
	}
	for i, example := range examples {
		data, err := configexamples.Files.ReadFile(example.Path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Spec struct {
				DisplayName string `yaml:"displayName"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Spec.DisplayName == "" {
			t.Fatalf("%s has no spec.displayName to describe it", example.Path)
		}
		if example.Description != doc.Spec.DisplayName {
			t.Errorf("%s description = %q, want %q", example.Name, example.Description, doc.Spec.DisplayName)
		}

		fields := strings.SplitN(lines[i], "  ", 2)
		if fields[0] != example.Name {
			t.Errorf("line %d name = %q, want %q", i, fields[0], example.Name)
		}
		if len(fields) != 2 || strings.TrimSpace(fields[1]) != doc.Spec.DisplayName {
			t.Errorf("line %d = %q, want description %q", i, lines[i], doc.Spec.DisplayName)
		}
	}
}

func TestExamplesShowPrintsExactEmbeddedYAML(t *testing.T) {
	examples, err := configexamples.WorkflowExamples()
	if err != nil {
		t.Fatal(err)
	}
	for _, example := range examples {
		t.Run(example.Name, func(t *testing.T) {
			want, err := configexamples.Files.ReadFile(example.Path)
			if err != nil {
				t.Fatal(err)
			}

			code, stdout, stderr := runArgs(t, "examples", "show", example.Name)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			if stdout != string(want) {
				t.Fatalf("stdout does not match embedded %s", example.Path)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestExamplesShowRejectsUnknownName(t *testing.T) {
	code, stdout, stderr := runArgs(t, "examples", "show", "../implementation")
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `unknown workflow example "../implementation"`) {
		t.Fatalf("stderr = %q, want unknown-name error", stderr)
	}
	if _, err := configexamples.ReadWorkflowExample("../implementation"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadWorkflowExample error = %v, want fs.ErrNotExist", err)
	}
}

func TestExamplesUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"examples"},
		{"examples", "bogus"},
		{"examples", "list", "extra"},
		{"examples", "show"},
		{"examples", "show", "implementation", "extra"},
	} {
		code, stdout, stderr := runArgs(t, args...)
		if code != 2 {
			t.Errorf("%v: code = %d, want 2", args, code)
		}
		if stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, stdout)
		}
		if !strings.Contains(stderr, "Usage: goobers examples") {
			t.Errorf("%v: stderr = %q, want examples usage", args, stderr)
		}
	}
}

func TestExamplesCompletionCandidates(t *testing.T) {
	got := completionCandidates("examples", t.TempDir())
	want := []string{"backlog-assignment", "backlog-curation", "implementation", "work-nomination"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}
