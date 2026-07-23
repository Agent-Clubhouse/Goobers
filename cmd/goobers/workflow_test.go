package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const workflowShowFixture = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: manual
  start: prepare
  tasks:
    - name: prepare
      type: deterministic
      goal: Prepare the change.
      run:
        command: ["true"]
      next: review
    - name: finish
      type: agentic
      goober: coder
      goal: Finish the change.
  gates:
    - name: review
      evaluator: human
      human: {}
      branches:
        pass: finish
        fail: prepare
`

func TestWorkflowShowPrintsTextDAG(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(workflowShowFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "workflow", "show", "default-implement", root)
	if code != 0 {
		t.Fatalf("workflow show: code = %d, stderr = %q", code, stderr)
	}

	want := `workflow: default-implement
triggers: manual-only
start: prepare
stages:
  prepare (kind: deterministic) -> review
  finish (kind: agentic) -> <complete>
  review (kind: gate, evaluator: human)
    pass target: finish
    fail target: prepare
`
	if stdout != want {
		t.Fatalf("workflow show stdout:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestWorkflowShowSurfacesValidationWarnings(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(workflowShowFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "workflow", "show", "default-implement", root)
	if code != 0 {
		t.Fatalf("workflow show: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "has no schedule trigger; it will not fire autonomously") {
		t.Fatalf("workflow show stderr did not surface validation warning: %q", stderr)
	}
}

// workflowDOTFixture is workflowShowFixture's gate swapped from human to
// automated (#706: human gates are rejected at compile time until durable
// pause/resume ships, and --dot now compiles the workflow to build its
// graph projection — unlike the plain text DAG above, which reads
// wf.Spec directly and never compiles, so workflowShowFixture's human gate
// stays valid for TestWorkflowShowPrintsTextDAG). Same node/edge topology
// (pass/fail branches to the same targets), so the expected DOT output is
// unaffected by the swap — DOT rendering has no evaluator-specific text.
const workflowDOTFixture = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: backlog-item
      selector:
        goobers: "true"
  start: prepare
  tasks:
    - name: prepare
      type: deterministic
      goal: Prepare the change.
      run:
        command: ["true"]
      next: review
    - name: finish
      type: agentic
      goober: coder
      goal: Finish the change.
  gates:
    - name: review
      evaluator: automated
      automated:
        check: tests-pass
      branches:
        pass: finish
        fail: prepare
`

func TestWorkflowShowPrintsDOT(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(workflowDOTFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "workflow", "show", "--dot", "default-implement", root)
	if code != 0 {
		t.Fatalf("workflow show --dot: code = %d, stderr = %q", code, stderr)
	}

	want := `digraph {
  "prepare" [shape=box];
  "prepare" -> "review";
  "finish" [shape=box];
  "finish" -> "<complete>";
  "review" [shape=diamond];
  "review" -> "finish" [label="pass"];
  "review" -> "prepare" [label="fail"];
}
`
	if stdout != want {
		t.Fatalf("workflow show --dot stdout:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestWorkflowShowUnknownWorkflow(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "workflow", "show", "no-such-workflow", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, `no workflow named "no-such-workflow"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestWorkflowUsage(t *testing.T) {
	code, _, stderr := runArgs(t, "workflow")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage: goobers workflow show [--dot] <name> [path]") {
		t.Fatalf("stderr = %q", stderr)
	}

	code, stdout, _ := runArgs(t, "workflow", "help")
	if code != 0 {
		t.Fatalf("workflow help code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "Usage: goobers workflow show [--dot] <name> [path]") {
		t.Fatalf("workflow help stdout = %q", stdout)
	}

	code, _, stderr = runArgs(t, "workflow", "bogus")
	if code != 2 {
		t.Fatalf("unknown subcommand code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown subcommand "bogus"`) {
		t.Fatalf("unknown subcommand stderr = %q", stderr)
	}

	code, stdout, _ = runArgs(t, "help")
	if code != 0 {
		t.Fatalf("help code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "goobers workflow show <name> [path]") {
		t.Fatalf("help stdout = %q", stdout)
	}
}
