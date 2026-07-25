package tutorclass

import "testing"

const workflowFixture = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: example
spec:
  gaggle: goobers
  triggers:
    - type: manual
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: build it
      run:
        command: ["true"]
      next: gate-a
    - name: publish
      type: deterministic
      goal: publish it
      run:
        command: ["true"]
  gates:
    - name: gate-a
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: publish
        fail: "@abort"
`

func replaceOnce(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestWorkflowTopologyChangedDetectsAddedTask(t *testing.T) {
	newYAML := replaceOnce(workflowFixture, "next: gate-a\n", "next: gate-a\n    - name: extra\n      type: deterministic\n      goal: extra\n      run:\n        command: [\"true\"]\n")
	changed, err := WorkflowTopologyChanged([]byte(workflowFixture), []byte(newYAML))
	if err != nil {
		t.Fatalf("WorkflowTopologyChanged: %v", err)
	}
	if !changed {
		t.Fatal("expected topology change for an added task")
	}
}

func TestWorkflowTopologyChangedDetectsRemovedGate(t *testing.T) {
	newYAML := replaceOnce(workflowFixture, `  gates:
    - name: gate-a
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: publish
        fail: "@abort"
`, "  gates: []\n")
	changed, err := WorkflowTopologyChanged([]byte(workflowFixture), []byte(newYAML))
	if err != nil {
		t.Fatalf("WorkflowTopologyChanged: %v", err)
	}
	if !changed {
		t.Fatal("expected topology change for a removed gate")
	}
}

func TestWorkflowTopologyChangedDetectsRewiredNext(t *testing.T) {
	newYAML := replaceOnce(workflowFixture, "next: gate-a\n", "next: publish\n")
	changed, err := WorkflowTopologyChanged([]byte(workflowFixture), []byte(newYAML))
	if err != nil {
		t.Fatalf("WorkflowTopologyChanged: %v", err)
	}
	if !changed {
		t.Fatal("expected topology change for rewired Next")
	}
}

func TestWorkflowTopologyChangedFalseForGateFieldTuning(t *testing.T) {
	newYAML := replaceOnce(workflowFixture, "check: status-equals", "check: output-numeric-lte")
	changed, err := WorkflowTopologyChanged([]byte(workflowFixture), []byte(newYAML))
	if err != nil {
		t.Fatalf("WorkflowTopologyChanged: %v", err)
	}
	if changed {
		t.Fatal("gate field tuning must not read as topology change")
	}
}

func TestWorkflowTopologyChangedFalseWhenUnchanged(t *testing.T) {
	changed, err := WorkflowTopologyChanged([]byte(workflowFixture), []byte(workflowFixture))
	if err != nil {
		t.Fatalf("WorkflowTopologyChanged: %v", err)
	}
	if changed {
		t.Fatal("identical revisions must not read as topology change")
	}
}

func TestWorkflowTopologyChangedFalseWhenFileAbsentAtOneRevision(t *testing.T) {
	changed, err := WorkflowTopologyChanged(nil, []byte(workflowFixture))
	if err != nil {
		t.Fatalf("WorkflowTopologyChanged: %v", err)
	}
	if changed {
		t.Fatal("a brand-new file must not itself read as a topology change")
	}
}

func TestGateFieldsChangedTrueForTuning(t *testing.T) {
	newYAML := replaceOnce(workflowFixture, "check: status-equals", "check: output-numeric-lte")
	changed, err := GateFieldsChanged([]byte(workflowFixture), []byte(newYAML))
	if err != nil {
		t.Fatalf("GateFieldsChanged: %v", err)
	}
	if !changed {
		t.Fatal("expected gate field change to be detected")
	}
}

func TestGateFieldsChangedFalseWhenUnchanged(t *testing.T) {
	changed, err := GateFieldsChanged([]byte(workflowFixture), []byte(workflowFixture))
	if err != nil {
		t.Fatalf("GateFieldsChanged: %v", err)
	}
	if changed {
		t.Fatal("identical revisions must report no gate field change")
	}
}

const gooberFixture = `apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: example
spec:
  gaggle: goobers
  role: coder
  instructions: instructions.md
  skills:
    - review-basics
    - go-conventions
`

func TestGooberSkillsChangedTrueForAddedSkill(t *testing.T) {
	newYAML := replaceOnce(gooberFixture, "- go-conventions\n", "- go-conventions\n    - new-skill\n")
	changed, err := GooberSkillsChanged([]byte(gooberFixture), []byte(newYAML))
	if err != nil {
		t.Fatalf("GooberSkillsChanged: %v", err)
	}
	if !changed {
		t.Fatal("expected skill addition to be detected")
	}
}

func TestGooberSkillsChangedFalseForReorder(t *testing.T) {
	newYAML := `apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: example
spec:
  gaggle: goobers
  role: coder
  instructions: instructions.md
  skills:
    - go-conventions
    - review-basics
`
	changed, err := GooberSkillsChanged([]byte(gooberFixture), []byte(newYAML))
	if err != nil {
		t.Fatalf("GooberSkillsChanged: %v", err)
	}
	if changed {
		t.Fatal("reordering the same skill set must not read as a change")
	}
}

func TestGooberSkillsChangedFalseWhenFileAbsentAtOneRevision(t *testing.T) {
	changed, err := GooberSkillsChanged(nil, []byte(gooberFixture))
	if err != nil {
		t.Fatalf("GooberSkillsChanged: %v", err)
	}
	if changed {
		t.Fatal("a brand-new goober file must not itself read as a skills change")
	}
}

func TestEscalate(t *testing.T) {
	cases := []struct{ a, b, want Category }{
		{CategoryPersona, CategoryGateTune, CategoryGateTune},
		{CategoryGateTune, CategoryStructure, CategoryStructure},
		{CategoryStructure, CategoryPersona, CategoryStructure},
		{CategoryPersona, CategoryPersona, CategoryPersona},
	}
	for _, c := range cases {
		if got := Escalate(c.a, c.b); got != c.want {
			t.Errorf("Escalate(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestRequiresSignoff(t *testing.T) {
	cases := []struct {
		category Category
		want     bool
	}{
		{CategoryPersona, false},
		{CategoryGateTune, false},
		{CategoryStructure, true},
	}
	for _, c := range cases {
		if got := c.category.RequiresSignoff(); got != c.want {
			t.Errorf("RequiresSignoff(%q) = %v, want %v", c.category, got, c.want)
		}
	}
}

func TestWorkflowTopologyChangedRejectsMalformedYAML(t *testing.T) {
	if _, err := WorkflowTopologyChanged([]byte("not: [valid"), []byte(workflowFixture)); err == nil {
		t.Fatal("expected an error for malformed old YAML")
	}
}

func TestGooberSkillsChangedRejectsMalformedYAML(t *testing.T) {
	if _, err := GooberSkillsChanged([]byte("not: [valid"), []byte(gooberFixture)); err == nil {
		t.Fatal("expected an error for malformed old YAML")
	}
}
