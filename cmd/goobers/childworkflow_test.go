package main

import (
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/childworkflow"
)

const childValidationParent = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: default-implement
  annotations: {goobers.dev/allow-preview-features: "true"}
spec:
  gaggle: example
  triggers: [{type: manual}]
  start: plan
  tasks:
    - name: plan
      type: agentic
      goober: coder
      goal: Plan a child workflow
      capabilities: [agent:model]
      childWorkflows:
        allowedGoobers: [coder]
        allowedCapabilities: [agent:model, repo:push]
        allowPRPublication: true
`

const childValidationProposal = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata: {name: generated-check}
spec:
  gaggle: example
  triggers: [{type: manual}]
  start: check
  tasks:
    - name: check
      type: deterministic
      goal: Run a local check
      run: {command: ["true"]}
`

func childValidationFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := initDemo(t)
	parent := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(parent, []byte(childValidationParent), 0o644); err != nil {
		t.Fatal(err)
	}
	proposal := filepath.Join(t.TempDir(), "child.yaml")
	if err := os.WriteFile(proposal, []byte(childValidationProposal), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, parent, proposal
}

func validateChildArgs(root, proposal string) []string {
	return []string{"workflow", "validate-child", "--gaggle", "example", "--parent", "default-implement", "--stage", "plan", "--json", proposal, root}
}

func TestWorkflowValidateChildProductionPathIsAdvisoryAndReadOnly(t *testing.T) {
	root, _, proposal := childValidationFixture(t)
	before := childValidationTree(t, root)
	proposalBefore, err := os.ReadFile(proposal)
	if err != nil {
		t.Fatal(err)
	}
	// If the validation path accidentally starts either runtime, this command
	// is not available and the check fails instead of doing network/model work.
	t.Setenv("PATH", t.TempDir())
	code, stdout, stderr := runArgs(t, validateChildArgs(root, proposal)...)
	if code != 0 {
		t.Fatalf("validate-child: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var report childworkflow.ConfiguredReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Valid || !report.Advisory || report.Scope != "current-config" || report.Parent.Stage != "plan" || report.SchemaVersion != "child-workflow-validation/v1" {
		t.Fatalf("unexpected report: %+v", report)
	}
	for _, digest := range []string{report.SourceDigest, report.CanonicalDigest, report.ConfigDigest, report.PolicyDigest, report.WorkflowDigest} {
		if !strings.HasPrefix(digest, "sha256:") {
			t.Fatalf("missing digest: %+v", report)
		}
	}
	if !reflect.DeepEqual(before, childValidationTree(t, root)) {
		t.Fatal("validation wrote instance configuration/runtime state")
	}
	proposalAfter, err := os.ReadFile(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if string(proposalBefore) != string(proposalAfter) {
		t.Fatal("validation modified proposal")
	}
}

func TestWorkflowValidateChildUsesConfiguredParentGrant(t *testing.T) {
	root, parent, proposal := childValidationFixture(t)
	denied := strings.Replace(childValidationProposal, "type: deterministic", "type: deterministic\n      capabilities: [repo:push]", 1)
	if err := os.WriteFile(proposal, []byte(denied), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := runArgs(t, validateChildArgs(root, proposal)...)
	if code != 1 || !strings.Contains(stdout, `"code": "capability"`) {
		t.Fatalf("parent capabilities failed to bound child policy: code=%d %s", code, stdout)
	}
	// The proposal cannot promote its own policy through open metadata.
	spoofed := strings.Replace(denied, "metadata: {name: generated-check}", "metadata: {name: generated-check, childWorkflows: {allowedCapabilities: [repo:push]}}", 1)
	if err := os.WriteFile(proposal, []byte(spoofed), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runArgs(t, validateChildArgs(root, proposal)...)
	if code != 1 || !strings.Contains(stdout, `"code": "capability"`) {
		t.Fatalf("proposal-supplied policy widened authority: %d %s", code, stdout)
	}
	// The same proposal becomes structurally authorized only after an explicit
	// edit to the selected trusted parent stage (the CLI still executes nothing).
	broader := strings.Replace(childValidationParent, "capabilities: [agent:model]", "capabilities: [agent:model, repo:push]", 1)
	if err := os.WriteFile(parent, []byte(broader), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, validateChildArgs(root, proposal)...)
	if code != 0 {
		t.Fatalf("configured grant was not used: %d %s %s", code, stdout, stderr)
	}
}

func TestWorkflowValidateChildRefusalsAndExitCodes(t *testing.T) {
	root, _, proposal := childValidationFixture(t)
	for _, tc := range []struct{ name, source, code string }{
		{"foreign gaggle", strings.Replace(childValidationProposal, "gaggle: example", "gaggle: other", 1), "scope"},
		{"invalid graph", strings.Replace(childValidationProposal, "start: check", "start: absent", 1), "compile"},
		{"duplicate key", strings.Replace(childValidationProposal, "gaggle: example", "gaggle: other\n  gaggle: example", 1), "syntax"},
		{"second resource", childValidationProposal + "\n---\nkind: Goober\n", "documents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(proposal, []byte(tc.source), 0o644); err != nil {
				t.Fatal(err)
			}
			code, stdout, _ := runArgs(t, validateChildArgs(root, proposal)...)
			if code != 1 || !strings.Contains(stdout, `"code": "`+tc.code+`"`) {
				t.Fatalf("code=%d report=%s", code, stdout)
			}
		})
	}
	if err := os.WriteFile(proposal, []byte(childValidationProposal), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, selection := range []struct {
		index int
		value string
	}{{3, "other"}, {5, "missing"}, {7, "missing"}} {
		args := validateChildArgs(root, proposal)
		args[selection.index] = selection.value
		code, stdout, _ := runArgs(t, args...)
		if code != 1 || !strings.Contains(stdout, `"code": "parent_config"`) {
			t.Fatalf("unconfigured selection accepted: %d %s", code, stdout)
		}
	}
	code, stdout, _ := runArgs(t, validateChildArgs(root, proposal+"-missing")...)
	if code != 2 || !strings.Contains(stdout, `"code": "io"`) {
		t.Fatalf("IO status=%d report=%s", code, stdout)
	}
	if code, _, _ := runArgs(t, "workflow", "validate-child", proposal, root); code != 2 {
		t.Fatalf("missing required flags: %d", code)
	}
}

func childValidationTree(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	files := map[string][32]byte{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[path] = [32]byte{}
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = sha256.Sum256(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
