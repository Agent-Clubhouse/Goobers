package interactiveops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

const dynamicWorkflow = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: arithmetic-review
  annotations:
    goobers.dev/acknowledge-manual-only: "true"
spec:
  gaggle: interactive
  displayName: Syntax then arithmetic regression
  triggers: [{type: manual}]
  readiness: {maxConcurrentRuns: 1}
  start: syntax
  tasks:
    - name: syntax
      type: deterministic
      goal: Check module syntax before running tests
      timeoutSeconds: 10
      next: regression
      run:
        workspace: scratch
        network: none
        command: [node, --check, src/arithmetic.mjs]
    - name: regression
      type: deterministic
      goal: Verify the reviewed arithmetic behavior
      timeoutSeconds: 20
      run:
        workspace: scratch
        network: none
        command: [node, --test, tests/arithmetic.test.mjs, tests/smoke.test.mjs]
`

func customRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for name, content := range map[string]string{
		"src/arithmetic.mjs":        "export const add=(a,b)=>a+b;\n",
		"tests/arithmetic.test.mjs": "import {test} from 'node:test'; import assert from 'node:assert/strict'; import {add} from '../src/arithmetic.mjs'; test('dynamic arithmetic result',()=>assert.equal(add(2,3),5));\n",
		"tests/smoke.test.mjs":      "import {test} from 'node:test'; test('dynamic second approved file',()=>{});\n",
	} {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}
func customService(t *testing.T, binary string) (*Service, Scope) {
	t.Helper()
	s, err := New(Options{Root: t.TempDir(), Repository: customRepository(t), Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	return s, Scope{ActorID: "author", SessionID: "custom-repository"}
}
func TestDynamicWorkflowValidationAndNativeConfig(t *testing.T) {
	s, scope := customService(t, "")
	ctx := context.Background()
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Workflows) != 1 || state.Workflows[0].Name != "repository-check" || strings.Contains(state.WorkflowDraft, "invoice") {
		t.Fatalf("custom repository still requires invoice: %+v", state)
	}
	v, err := s.ValidateWorkflow(ctx, scope, dynamicWorkflow)
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	if len(v.Commands) != 2 || v.Commands[0].Capability != CommandNodeCheck || v.Commands[1].Capability != CommandNodeTest || strings.Contains(v.NormalizedYAML, state.Repository) || !strings.Contains(v.BoundYAML, state.Repository) || v.PayloadDigest != digest([]byte(v.NormalizedYAML)) {
		t.Fatalf("portable/bound contract: %+v", v)
	}
	again, err := s.ValidateWorkflow(ctx, scope, v.NormalizedYAML)
	if err != nil || again.PayloadDigest != v.PayloadDigest || again.BindingDigest != v.BindingDigest {
		t.Fatalf("normalization is unstable: %+v %v", again, err)
	}
	a, err := s.Propose(ctx, scope, Request{Kind: "add_workflow", WorkflowYAML: v.NormalizedYAML, IdempotencyKey: "dynamic"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Preview != v.BoundYAML {
		t.Fatal("preview does not bind exact native payload")
	}
	a, err = s.Apply(ctx, scope, a.ID)
	if err != nil || a.Status != "succeeded" {
		t.Fatalf("apply: %+v %v", a, err)
	}
	set, _, err := instance.LoadConfigDir(filepath.Join(state.InstanceRoot, "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, wf := range set.Workflows {
		if wf.Name == v.Name {
			if len(wf.Spec.Tasks) != 2 || wf.Spec.Tasks[0].Next != "regression" || wf.Spec.Tasks[0].Run.Command[2] != filepath.Join(state.Repository, "src/arithmetic.mjs") {
				t.Fatalf("native graph changed: %+v", wf)
			}
			return
		}
	}
	t.Fatal("authored native workflow missing")
}
func TestDynamicWorkflowRejectsAuthorityExpansion(t *testing.T) {
	s, scope := customService(t, "")
	ctx := context.Background()
	cases := map[string]string{
		"multiple-documents": dynamicWorkflow + "\n---\nkind: Secret\n",
		"raw-shell":          strings.Replace(dynamicWorkflow, "[node, --check, src/arithmetic.mjs]", "[sh, -c, src/arithmetic.mjs]", 1),
		"node-import":        strings.Replace(dynamicWorkflow, "[node, --check, src/arithmetic.mjs]", "[node, --import, src/arithmetic.mjs]", 1),
		"extra-flag":         strings.Replace(dynamicWorkflow, "tests/smoke.test.mjs]", "--watch]", 1),
		"traversal":          strings.Replace(dynamicWorkflow, "src/arithmetic.mjs", "../src/arithmetic.mjs", 1),
		"foreign-absolute":   strings.Replace(dynamicWorkflow, "src/arithmetic.mjs", "/tmp/foreign.mjs", 1),
		"environment":        strings.Replace(dynamicWorkflow, "network: none", "network: none\n        env: {TOKEN: fake}", 1),
		"script":             strings.Replace(dynamicWorkflow, "network: none", "network: none\n        script: echo hacked", 1),
		"credential":         strings.Replace(dynamicWorkflow, "      run:", "      capabilities: [repo:push]\n      run:", 1),
		"loop":               strings.Replace(dynamicWorkflow, "next: regression", "next: syntax", 1),
		"unreachable":        strings.Replace(dynamicWorkflow, "      next: regression\n", "", 1),
		"missing-stage":      strings.Replace(dynamicWorkflow, "next: regression", "next: absent", 1),
		"unbounded":          strings.Replace(dynamicWorkflow, "timeoutSeconds: 10", "timeoutSeconds: 31", 1),
		"network":            strings.Replace(dynamicWorkflow, "network: none", "network: public", 1),
		"schema":             strings.Replace(dynamicWorkflow, "name: syntax", "name: 'invalid name'", 1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := s.ValidateWorkflow(ctx, scope, content)
			if err != nil {
				t.Fatal(err)
			}
			if v.Valid || len(v.Diagnostics) != 1 || v.Diagnostics[0].Severity != "error" {
				t.Fatalf("missing normalized error: %+v", v)
			}
			if _, err = s.Propose(ctx, scope, Request{Kind: "add_workflow", WorkflowYAML: content, IdempotencyKey: name}); err == nil {
				t.Fatal("invalid authoring proposed")
			}
		})
	}
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(state.Repository, "src/arithmetic.mjs"), filepath.Join(state.Repository, "src/link.mjs")); err != nil {
		t.Fatal(err)
	}
	v, err := s.ValidateWorkflow(ctx, scope, strings.Replace(dynamicWorkflow, "src/arithmetic.mjs", "src/link.mjs", 1))
	if err != nil || v.Valid || v.Diagnostics[0].Code != "path" {
		t.Fatalf("symlink accepted: %+v %v", v, err)
	}
}
func TestDynamicHostCapabilitiesAndScopeBinding(t *testing.T) {
	ctx := context.Background()
	repo := customRepository(t)
	s, err := New(Options{Root: t.TempDir(), RepositoryForScope: func(Scope) (string, error) { return repo, nil }, CommandCapabilities: []CommandCapability{CommandNodeCheck}})
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{ActorID: "alice", SessionID: "bound"}
	v, err := s.ValidateWorkflow(ctx, scope, dynamicWorkflow)
	if err != nil || v.Valid || v.Diagnostics[0].Code != "authority" {
		t.Fatalf("capability denial: %+v %v", v, err)
	}
	name := "rename"
	a, err := s.Propose(ctx, scope, Request{Kind: "tweak_gaggle", DisplayName: &name, IdempotencyKey: "before-swap"})
	if err != nil {
		t.Fatal(err)
	}
	repo = customRepository(t)
	if _, err = s.Apply(ctx, scope, a.ID); err == nil || !strings.Contains(err.Error(), "authority changed") {
		t.Fatalf("scope swap applied: %v", err)
	}
	if _, err = New(Options{Root: t.TempDir(), CommandCapabilities: []CommandCapability{"shell"}}); err == nil {
		t.Fatal("unregistered host capability accepted")
	}
	empty, err := New(Options{Root: t.TempDir(), Repository: t.TempDir(), CommandCapabilities: []CommandCapability{}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := empty.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Workflows) != 0 || state.WorkflowDraft != "" {
		t.Fatal("empty custom source invented workflow")
	}
}
func TestDynamicChildApprovalRejectsBeforeEffects(t *testing.T) {
	s, scope := customService(t, "")
	ctx := context.Background()
	v, err := s.ValidateWorkflow(ctx, scope, dynamicWorkflow)
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	child := Scope{ActorID: scope.ActorID, SessionID: "independent-child"}
	c, err := s.ValidateWorkflow(ctx, child, v.NormalizedYAML)
	if err != nil || !c.Valid || c.PayloadDigest != v.PayloadDigest || c.BindingDigest == v.BindingDigest {
		t.Fatalf("child snapshot binding: %+v %v", c, err)
	}
	_, err = s.RunReviewChildWithOptions(ctx, child, "review", "descriptive only", ReviewChildOptions{WorkflowYAML: v.NormalizedYAML, ApprovedPayloadDigest: strings.Repeat("0", 64)}, nil)
	if err == nil || !strings.Contains(err.Error(), "approved") {
		t.Fatalf("mismatch not rejected: %v", err)
	}
	state, err := s.Snapshot(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Actions) != 0 {
		t.Fatal("approval mismatch had effects")
	}
}
