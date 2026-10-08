package workflow

import (
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func compileRebindWorkflow(t *testing.T, doc string) *Machine {
	t.Helper()
	var spec apiv1.WorkflowSpec
	if err := yaml.UnmarshalStrict([]byte(doc), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if spec.Gaggle == "" {
		spec.Gaggle = "web"
	}
	if len(spec.Triggers) == 0 {
		spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}
	}
	m, err := compileAcknowledged(Definition{Name: "review", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return m
}

func TestReadOnlyWorkspacesAfterRebind(t *testing.T) {
	const gate = `
gates:
  - name: review
    evaluator: agentic
    agentic: {goober: reviewer, workspace: %s}
    branches: {pass: "", fail: "@abort", needs-changes: "@abort"}
`
	tests := []struct {
		name string
		doc  string
		want []ReadOnlyAfterRebind
	}{
		{
			name: "gather-sibling-context before a read-only gate",
			doc: `
start: gather
tasks:
  - name: gather
    type: deterministic
    goal: gather
    run: {command: [goobers, gather-sibling-context]}
    capabilities: [github:pr:write]
    policyActions: [flag-scope-drift, route-verdict]
    next: review
` + strings.ReplaceAll(gate, "%s", "repo-readonly"),
			want: []ReadOnlyAfterRebind{{Stage: "review", Kind: "gate", Field: "agentic.workspace", Rebinders: []string{"gather"}}},
		},
		{
			name: "declared workspaceBranch output before read-only tasks",
			doc: `
start: select
tasks:
  - name: select
    type: deterministic
    goal: select
    run: {command: [./select.sh]}
    expectedOutputs: [workspaceBranch]
    next: lint
  - name: lint
    type: deterministic
    goal: lint
    run: {command: [make, lint], workspace: repo-readonly}
    next: inspect
  - name: inspect
    type: agentic
    goal: inspect
    goober: reviewer
    workspace: repo-readonly
    next: ""
`,
			want: []ReadOnlyAfterRebind{
				{Stage: "lint", Kind: "task", Field: "run.workspace", Rebinders: []string{"select"}},
				{Stage: "inspect", Kind: "task", Field: "workspace", Rebinders: []string{"select"}},
			},
		},
		{
			name: "read-only gate before the rebinder",
			doc: `
start: review
tasks:
  - name: gather
    type: deterministic
    goal: gather
    run: {command: [goobers, gather-pr-context]}
    capabilities: [github:pr:write, repo:push]
    next: ""
gates:
  - name: review
    evaluator: agentic
    agentic: {goober: reviewer, workspace: repo-readonly}
    branches: {pass: gather, fail: "@abort", needs-changes: "@abort"}
`,
		},
		{
			name: "writable gate after the rebinder",
			doc: `
start: gather
tasks:
  - name: gather
    type: deterministic
    goal: gather
    run: {command: [goobers, gather-pr-context]}
    capabilities: [github:pr:write, repo:push]
    next: review
` + strings.ReplaceAll(gate, "%s", "repo"),
		},
		{
			name: "push-branch does not rebind",
			doc: `
start: push
tasks:
  - name: push
    type: deterministic
    goal: push
    run: {command: [goobers, push-branch]}
    capabilities: [repo:push]
    policyActions: [push-repository-branch]
    next: review
` + strings.ReplaceAll(gate, "%s", "repo-readonly"),
		},
		{
			name: "agentic workspaceBranch output is not honoured",
			doc: `
start: pick
tasks:
  - name: pick
    type: agentic
    goal: pick
    goober: coder
    expectedOutputs: [workspaceBranch]
    next: review
` + strings.ReplaceAll(gate, "%s", "repo-readonly"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReadOnlyWorkspacesAfterRebind(compileRebindWorkflow(t, tt.doc))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("findings = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReadOnlyAfterRebindThroughParallelJoin(t *testing.T) {
	m := compileRebindWorkflow(t, `
start: fan
tasks:
  - name: gather
    type: deterministic
    goal: gather
    run: {command: [./select.sh], workspace: scratch}
    expectedOutputs: [workspaceBranch]
    next: "@join"
  - name: other
    type: deterministic
    goal: other
    run: {command: [make, build], workspace: scratch}
    next: "@join"
gates:
  - name: review
    evaluator: agentic
    agentic: {goober: reviewer, workspace: repo-readonly}
    branches: {pass: "", fail: "@abort", needs-changes: "@abort"}
parallels:
  - name: fan
    failurePolicy: fail_fast
    branches:
      - {name: a, start: gather}
      - {name: b, start: other}
    join: review
    onFailure: "@abort"
`)
	got := WorkspaceRebindersReaching(m, "review")
	if !reflect.DeepEqual(got, []string{"gather"}) {
		t.Fatalf("rebinders = %v, want [gather]", got)
	}
	findings := ReadOnlyWorkspacesAfterRebind(m)
	if len(findings) != 1 {
		t.Fatalf("findings = %#v, want one", findings)
	}
	msg := findings[0].Message("review")
	for _, want := range []string{`gate "review"`, `stage "gather"`, "Move \"review\" before", "agentic.workspace: repo"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
}
