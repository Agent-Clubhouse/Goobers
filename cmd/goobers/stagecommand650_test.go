package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStageCommandProblemResolution unit-tests the #650 CLI-surface resolver
// against the live command registry: a real verb (and its positional args)
// passes, a renamed/typo'd verb is caught, and a command group's subcommand is
// resolved without false-flagging a runnable command's positional arguments.
func TestStageCommandProblemResolution(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantErr string // substring the single problem must contain; "" means no problem
	}{
		{"non-goobers shell command is out of scope", []string{"bash", "-c", "echo hi"}, ""},
		{"real provider stage verb", []string{"goobers", "backlog-query", "--claim"}, ""},
		{"real connector stage verb", []string{"goobers", "telemetry-query", "--window", "24h"}, ""},
		{"unknown top-level verb", []string{"goobers", "backlog-quiery", "--claim"}, `unknown goobers verb "backlog-quiery"`},
		{"run takes a workflow positional, not a subcommand", []string{"goobers", "run", "default-implement"}, ""},
		{"run abort is a real subcommand", []string{"goobers", "run", "abort", "some-run"}, ""},
		{"group subcommand resolves", []string{"goobers", "claims", "list"}, ""},
		{"group subcommand typo is caught", []string{"goobers", "claims", "lst"}, `unknown "claims" subcommand "lst"`},
		{"bare group needs a subcommand", []string{"goobers", "claims"}, `needs a subcommand for the "claims" command group`},
		{"bare goobers names no subcommand", []string{"goobers"}, "names no goobers subcommand"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := stageCommandProblem("wf", "task", tc.argv)
			if tc.wantErr == "" {
				if len(problems) != 0 {
					t.Fatalf("argv %v: got problems %v, want none", tc.argv, problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("argv %v: got %d problems %v, want exactly one", tc.argv, len(problems), problems)
			}
			if !strings.Contains(problems[0], tc.wantErr) {
				t.Fatalf("argv %v: problem = %q, want it to contain %q", tc.argv, problems[0], tc.wantErr)
			}
		})
	}
}

// TestValidateRejectsUnknownStageCommand is the #650 end-to-end regression:
// `goobers validate` must fail a config whose deterministic stage invokes a
// goobers verb that does not exist, with a clear per-stage diagnostic — rather
// than compiling clean and only failing once the runner shells out mid-run.
func TestValidateRejectsUnknownStageCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code = %d, stderr = %q", code, stderr)
	}

	// The seeded default-implement workflow runs `goobers backlog-query --claim`
	// as its first deterministic stage. Drift that verb to one that does not
	// exist; every other structural aspect stays valid so the #650 check (which
	// runs after a successful compile) is actually reached.
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	original, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	const realCommand = `["goobers", "backlog-query", "--claim"]`
	if !strings.Contains(string(original), realCommand) {
		t.Fatalf("seeded workflow no longer contains %s; update this test", realCommand)
	}
	drifted := strings.Replace(string(original), realCommand, `["goobers", "backlog-quiery", "--claim"]`, 1)
	if err := os.WriteFile(workflowPath, []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate: code = %d, want 1; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `unknown goobers verb "backlog-quiery"`) {
		t.Fatalf("validate stdout = %q, want it to name the unknown verb", stdout)
	}
	if !strings.Contains(stdout, "config references CLI stage commands that do not exist") {
		t.Fatalf("validate stdout = %q, want the summary line", stdout)
	}
}
