package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/version"
)

func runArgs(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// unsetRunContext clears any ambient GOOBERS_RUN_ID and GOOBERS_WORKFLOW for the
// test's duration, with restore. A test exercising the genuinely-unset
// (fail-closed) run-context path must not be defeated by these leaking in from
// the parent process — which is exactly what happens under a live `local-ci`
// stage: internal/executor.buildStageEnv injects both the run's real
// GOOBERS_RUN_ID and GOOBERS_WORKFLOW (env.go:65), and `make ci`'s
// `go test ./...` inherits them, so these tests saw ambient values, skipped the
// fail-closed branch, and failed on every run regardless of the implementer's
// diff (#321). Both are cleared because the tests' stated precondition is that
// both are absent. t.Setenv can only set, never unset, so this is explicit
// os.Unsetenv + os.Setenv restore.
func unsetRunContext(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GOOBERS_RUN_ID", "GOOBERS_WORKFLOW"} {
		orig, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		key := key
		t.Cleanup(func() { _ = os.Setenv(key, orig) })
	}
}

func TestRunNoArgs(t *testing.T) {
	code, _, stderr := runArgs(t)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("expected usage in stderr, got %q", stderr)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	code, _, stderr := runArgs(t, "bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown command "bogus"`) {
		t.Fatalf("stderr = %q, want it to mention the unknown command", stderr)
	}
}

func TestRunHelp(t *testing.T) {
	code, stdout, _ := runArgs(t, "help")
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "goobers init") {
		t.Fatalf("expected help text, got %q", stdout)
	}
}

func TestRunVersion(t *testing.T) {
	want := "goobers " + version.Get().String() + "\n"
	for _, arg := range []string{"--version", "version"} {
		t.Run(arg, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, arg)
			if code != 0 {
				t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
			}
			if stdout != want {
				t.Fatalf("stdout = %q, want %q", stdout, want)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestInitThenValidate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")

	code, stdout, stderr := runArgs(t, "init", root)
	if code != 0 {
		t.Fatalf("init: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "initialized instance at") {
		t.Fatalf("init stdout = %q", stdout)
	}

	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "OK:") {
		t.Fatalf("validate stdout = %q", stdout)
	}

	// Re-running init is a no-op, not an error.
	code, stdout, _ = runArgs(t, "init", root)
	if code != 0 {
		t.Fatalf("second init: code = %d", code)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Fatalf("second init stdout = %q", stdout)
	}
}

// TestValidateRejectsUnknownAutomatedCheckName is the regression test for
// #124: `goobers validate` previously never called workflow.Compile at all,
// so an AutomatedGate.Check typo (a real check name that just isn't
// registered) passed validation clean and only surfaced once a run actually
// reached that gate. It now compiles every workflow (compiledMachines, wired
// into runValidate) with WithKnownChecks against internal/gate.DefaultChecks,
// catching this at validate time.
func TestValidateRejectsUnknownAutomatedCheckName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code = %d, stderr = %q", code, stderr)
	}

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	broken := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: backlog-item
      selector:
        goobers: "true"
  readiness:
    maxConcurrentRuns: 1
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Implement the backlog item and open a PR.
      next: done-check
  gates:
    - name: done-check
      evaluator: automated
      automated:
        check: bogus-check
      branches:
        pass: ""
        fail: "@abort"
`
	if err := os.WriteFile(workflowPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code == 0 {
		t.Fatalf("validate: code = 0, want non-zero for an unknown check name; stdout = %q, stderr = %q", stdout, stderr)
	}
	if !strings.Contains(stdout, `unknown automated check "bogus-check"`) {
		t.Fatalf("validate stdout = %q, want it to mention the unknown check", stdout)
	}
}

func TestValidateWarnsForClaimStageWithoutResultFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code = %d, stderr = %q", code, stderr)
	}

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	workflow := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@hourly"
  start: query-backlog
  tasks:
    - name: query-backlog
      type: deterministic
      goal: Claim one backlog item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
`
	if err := os.WriteFile(workflowPath, []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate warning should be non-fatal: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "WARNING") || !strings.Contains(stdout, `task "query-backlog"`) ||
		!strings.Contains(stdout, "inputs.resultFile") {
		t.Fatalf("validate stdout = %q, want an actionable resultFile warning", stdout)
	}
}

// TestInitThenSelfhostValidates is issue #28's own acceptance criterion,
// literally: `goobers init` + the self-hosting dogfood config ->
// `goobers validate` passes, with every gaggle/goober/workflow resolving.
func TestInitThenSelfhostValidates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "selfhost-instance")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code = %d, stderr = %q", code, stderr)
	}

	// Replace the generic seeded config with the real self-hosting config.
	configDir := filepath.Join(root, "config")
	if err := os.RemoveAll(configDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(configDir, os.DirFS("../../selfhost")); err != nil {
		t.Fatal(err)
	}
	// The blanket copy also pulls in files that aren't config-as-code
	// objects (the operator guide and the instance.yaml template) — remove
	// them from config/ so only Manifest/Gaggle/Goober/Workflow objects
	// remain, matching what a maintainer following the README would end up
	// with.
	_ = os.Remove(filepath.Join(configDir, "README.md"))
	instanceYAML, err := os.ReadFile(filepath.Join(configDir, "instance.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(configDir, "instance.yaml.example")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), instanceYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "1 gaggle(s), 6 goober(s), 6 workflow(s)") {
		t.Fatalf("validate stdout = %q, want all self-hosting objects to resolve", stdout)
	}
}

func TestValidateMissingInstance(t *testing.T) {
	root := t.TempDir()
	code, _, stderr := runArgs(t, "validate", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestValidateTooManyArgs(t *testing.T) {
	code, _, _ := runArgs(t, "validate", "a", "b")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}
