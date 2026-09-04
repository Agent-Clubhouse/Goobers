package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fixTestWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4"
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@every 24h"
  start: poll
  tasks:
    - name: poll
      type: deterministic
      goal: Poll CI.
      run:
        command: ["goobers", "ci-poll"]
      inputs:
        kind: "ci-poll"
        prNumber: "1"
      capabilities:
        - provider:pr:write
      next: ci
  gates:
    - name: ci
      evaluator: automated
      automated:
        check: ci-status
      branches:
        pass: ""
        fail: "@abort"
        timeout: "@escalate"
`

func initFixTestInstance(t *testing.T) (root, workflowPath string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "foreign")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	workflowPath = filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(fixTestWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, workflowPath
}

func TestFixDryRunPrintsDiffWithoutWriting(t *testing.T) {
	root, workflowPath := initFixTestInstance(t)

	code, stdout, stderr := runArgs(t, "fix", "--to", "2.0", root)
	if code != 0 {
		t.Fatalf("fix --to 2.0: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"FIX Workflow/default-implement",
		"migrated to dslVersion 2.0 (dry run",
		`gate "ci": pinned automated.pollIntervalSeconds: 10`,
		"-dslVersion: \"1.4\"",
		"+dslVersion: \"2.0\"",
		"+        pollIntervalSeconds: 10",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("fix --to 2.0 output missing %q:\n%s", want, stdout)
		}
	}

	after, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `dslVersion: "1.4"`) {
		t.Fatalf("dry run must not modify the file:\n%s", after)
	}
}

func TestFixWriteAppliesMigration(t *testing.T) {
	root, workflowPath := initFixTestInstance(t)

	code, stdout, stderr := runArgs(t, "fix", "--to", "2.0", "--write", root)
	if code != 0 {
		t.Fatalf("fix --to 2.0 --write: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "migrated to dslVersion 2.0 (written)") {
		t.Fatalf("fix --write output missing confirmation:\n%s", stdout)
	}

	after, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `dslVersion: "2.0"`) {
		t.Fatalf("--write did not bump dslVersion:\n%s", after)
	}
	if !strings.Contains(string(after), "pollIntervalSeconds: 10") {
		t.Fatalf("--write did not pin pollIntervalSeconds:\n%s", after)
	}

	// Idempotent: running fix --to 2.0 again now refuses (already at target).
	code, stdout, _ = runArgs(t, "fix", "--to", "2.0", "--write", root)
	if code != 0 {
		t.Fatalf("second fix --to 2.0: code=%d stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "already at the target dslVersion") {
		t.Fatalf("second fix --to 2.0 output = %q, want an already-at-target notice", stdout)
	}
}

func TestFixRefusesNonAdjacentVersion(t *testing.T) {
	root, _ := initFixTestInstance(t)

	code, stdout, stderr := runArgs(t, "fix", "--to", "3.0", root)
	if code != 1 {
		t.Fatalf("fix --to 3.0: code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no direct migration registered") {
		t.Fatalf("fix --to 3.0 output missing no-direct-edge diagnostic:\n%s", stdout)
	}
}

func TestFixRequiresToFlag(t *testing.T) {
	root, _ := initFixTestInstance(t)

	code, _, stderr := runArgs(t, "fix", root)
	if code != 2 {
		t.Fatalf("fix (no --to): code=%d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "--to <version> is required") {
		t.Fatalf("fix (no --to) stderr missing usage diagnostic: %q", stderr)
	}
}

func TestFixWriteOnReadOnlyDirLeavesSourceUnmodified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not gate file creation on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root, workflowPath := initFixTestInstance(t)
	before, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	workflowDir := filepath.Dir(workflowPath)
	if err := os.Chmod(workflowDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workflowDir, 0o700) })

	code, stdout, stderr := runArgs(t, "fix", "--to", "2.0", "--write", root)
	if code != 1 {
		t.Fatalf("fix --write on a read-only dir: code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "write:") {
		t.Fatalf("fix --write failure output missing a write diagnostic:\n%s", stdout)
	}

	after, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed --write must leave the source byte-identical:\n%s", after)
	}
}
