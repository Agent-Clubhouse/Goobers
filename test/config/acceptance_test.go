// Package config_acceptance is the M5 config-validation acceptance suite. It is an
// ADVERSARIAL stress layer on top of Dev-1's in-package validator tests: it throws
// configs that violate distinct state-machine / gate invariants at validate.ValidateDir
// and asserts each is rejected with a relevant error. These target rules the single
// shipped bad fixture does not exercise (GT-016 one-evaluator, duplicate states,
// dangling next/branch targets) so a regression in any of them fails CI here.
//
// Deliberately NOT a copy of cmd/validate/main_test.go (which already covers the
// happy-path example + exit codes) — this adds breadth, not duplication.
package config_acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
)

const (
	manifestYAML = `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: test-instance
spec:
  instance:
    name: test
    environment: dev
  gaggles:
    - web
`
	gaggleYAML = `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: web
spec:
  project:
    provider: github
    owner: acme
    name: web
  backlog:
    provider: github
    project: acme/web
  isolation:
    namespace: gaggle-web
`
	gooberYAML = `apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: coder
spec:
  gaggle: web
  role: coder
  instructions: instructions.md
  workflows:
    - flow
`
	instructionsMD = "# Coder\nImplement backlog items.\n"

	// validWorkflow: implement (agentic) -> review gate -> pass:finalize /
	// needs-changes:implement / fail:implement. All three must be present —
	// an agentic gate's reviewer can produce any of the closed
	// pass/fail/needs-changes decision set (#124), so a real base fixture
	// leaving one uncovered is itself invalid, not just this test's problem.
	// All states defined; gate has exactly one evaluator. This must validate clean.
	validWorkflow = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: flow
spec:
  gaggle: web
  triggers:
    - type: backlog-item
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Implement the item.
      next: review
    - name: finalize
      type: deterministic
      goal: Finalize.
      run:
        command: ["echo", "done"]
  gates:
    - name: review
      evaluator: agentic
      agentic:
        goober: coder
      branches:
        pass: finalize
        needs-changes: implement
        fail: implement
`
)

// writeConfig materializes a complete config dir in a temp dir, using the shared
// valid base files plus the supplied workflow YAML. Returns the dir root.
func writeConfig(t *testing.T, workflow string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"manifest.yaml":                             manifestYAML,
		"gaggles/web/gaggle.yaml":                   gaggleYAML,
		"gaggles/web/goobers/coder/goober.yaml":     gooberYAML,
		"gaggles/web/goobers/coder/instructions.md": instructionsMD,
		"gaggles/web/workflows/flow.yaml":           workflow,
	}
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

func errorMessages(t *testing.T, dir string) []string {
	t.Helper()
	v, err := validate.New()
	if err != nil {
		t.Fatalf("validate.New: %v", err)
	}
	report, err := v.ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	var msgs []string
	for _, i := range report.Issues {
		if i.Severity == validate.Error {
			msgs = append(msgs, i.Message)
		}
	}
	return msgs
}

// TestValidBaseConfigPasses is the positive control — the shared base must validate
// clean, so any failure below is attributable to the injected defect, not the base.
func TestValidBaseConfigPasses(t *testing.T) {
	msgs := errorMessages(t, writeConfig(t, validWorkflow))
	if len(msgs) != 0 {
		t.Fatalf("valid base config reported errors: %v", msgs)
	}
}

// TestGT016TwoEvaluatorsRejected pins the invariant that a gate with more than one
// evaluator block is rejected (GT-016 — one evaluator per gate). We assert REJECTION
// only, not the message, because this is currently caught at the JSON-Schema layer
// (workflow.schema.json gate allOf/not), not by the Go cross-ref validator.
//
// QA finding (reported to Dev-1, non-blocking): because ValidateDir drops a
// schema-invalid object before cross-ref, the clear validator message ("gate must
// have exactly one evaluator block") never fires; the user instead sees a cryptic
// schema error ("/spec/gates/0: not failed") PLUS a misleading cascade error blaming
// the goober ("associated workflow ... is not defined"). Correctness is fine; the
// message UX is poor. This test is intentionally not coupled to that wording so it
// won't break when the message is improved.
func TestGT016TwoEvaluatorsRejected(t *testing.T) {
	twoEval := strings.Replace(validWorkflow,
		`      evaluator: agentic
      agentic:
        goober: coder`,
		`      evaluator: agentic
      agentic:
        goober: coder
      automated:
        check: tests-pass`, 1)
	if twoEval == validWorkflow {
		t.Fatal("test setup: gate mutation did not change the YAML")
	}
	if msgs := errorMessages(t, writeConfig(t, twoEval)); len(msgs) == 0 {
		t.Fatal("a gate with two evaluator blocks must be rejected (GT-016), got no errors")
	}
}

func TestAdversarialConfigsRejected(t *testing.T) {
	cases := []struct {
		name     string
		workflow string
		wantSub  string // substring expected in at least one Error message
	}{
		{
			name: "duplicate_state_name",
			workflow: strings.Replace(validWorkflow,
				`    - name: finalize
      type: deterministic
      goal: Finalize.
      run:
        command: ["echo", "done"]`,
				`    - name: implement
      type: deterministic
      goal: Dup.
      run:
        command: ["echo", "dup"]`, 1),
			wantSub: "duplicate state name",
		},
		{
			name:     "dangling_task_next",
			workflow: strings.Replace(validWorkflow, "next: review", "next: nowhere", 1),
			wantSub:  "next state",
		},
		{
			name:     "dangling_gate_branch",
			workflow: strings.Replace(validWorkflow, "pass: finalize", "pass: nowhere", 1),
			wantSub:  "is not a defined state",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.workflow == validWorkflow {
				t.Fatalf("test setup: workflow mutation did not change the YAML (substring not found)")
			}
			msgs := errorMessages(t, writeConfig(t, tc.workflow))
			if len(msgs) == 0 {
				t.Fatalf("expected validation error containing %q, got none", tc.wantSub)
			}
			found := false
			for _, m := range msgs {
				if strings.Contains(m, tc.wantSub) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("no error contained %q; got: %v", tc.wantSub, msgs)
			}
		})
	}
}
