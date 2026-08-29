package failfirst

import (
	"errors"
	"testing"
)

const workflowNoGates = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: example
spec:
  gaggle: goobers
  triggers:
    - type: manual
  start: a
  tasks:
    - name: a
      type: deterministic
      run:
        command: ["true"]
`

const workflowOneGate = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: example
spec:
  gaggle: goobers
  triggers:
    - type: manual
  start: a
  tasks:
    - name: a
      type: deterministic
      run:
        command: ["true"]
      next: b
  gates:
    - name: a-valid
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: a
        fail: "@abort"
`

const workflowTwoGates = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: example
spec:
  gaggle: goobers
  triggers:
    - type: manual
  start: a
  tasks:
    - name: a
      type: deterministic
      run:
        command: ["true"]
      next: b
  gates:
    - name: a-valid
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: a
        fail: "@abort"
    - name: b-valid
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: a
        fail: "@abort"
`

func TestIsWorkflowFile(t *testing.T) {
	cases := map[string]bool{
		"reference-workflows/gaggles/goobers/workflows/tutor.yaml": true,
		"workflows/tutor.yaml": true,
		"workflows/tutor.yml":  true,
		"reference-workflows/gaggles/goobers/goobers/analyst.yaml": false,
		"reference-workflows/gaggles/goobers/workflows/README.md":  false,
		"docs/design/tutor-redesign.md":                            false,
	}
	for p, want := range cases {
		if got := IsWorkflowFile(p); got != want {
			t.Errorf("IsWorkflowFile(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestGateNamesEmptyContent(t *testing.T) {
	names, err := GateNames(nil)
	if err != nil {
		t.Fatalf("GateNames(nil): %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("GateNames(nil) = %v, want empty", names)
	}
}

func TestGateNamesMalformed(t *testing.T) {
	if _, err := GateNames([]byte("not: [valid\n")); err == nil {
		t.Fatal("GateNames(malformed) = nil error, want error")
	}
}

func TestNewGates_NoneAdded(t *testing.T) {
	added, err := NewGates("workflows/example.yaml", []byte(workflowOneGate), []byte(workflowOneGate))
	if err != nil {
		t.Fatalf("NewGates: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("NewGates (no change) = %v, want none", added)
	}
}

func TestNewGates_OneAdded(t *testing.T) {
	added, err := NewGates("workflows/example.yaml", []byte(workflowNoGates), []byte(workflowOneGate))
	if err != nil {
		t.Fatalf("NewGates: %v", err)
	}
	if len(added) != 1 || added[0].Gate != "a-valid" || added[0].Workflow != "example" {
		t.Fatalf("NewGates = %+v, want exactly one added gate a-valid on workflow example", added)
	}
}

func TestNewGates_OnlyReportsNetNew(t *testing.T) {
	added, err := NewGates("workflows/example.yaml", []byte(workflowOneGate), []byte(workflowTwoGates))
	if err != nil {
		t.Fatalf("NewGates: %v", err)
	}
	if len(added) != 1 || added[0].Gate != "b-valid" {
		t.Fatalf("NewGates = %+v, want exactly one added gate b-valid (a-valid pre-existed)", added)
	}
}

func TestNewGates_BrandNewFile(t *testing.T) {
	added, err := NewGates("workflows/example.yaml", nil, []byte(workflowOneGate))
	if err != nil {
		t.Fatalf("NewGates: %v", err)
	}
	if len(added) != 1 || added[0].Gate != "a-valid" {
		t.Fatalf("NewGates(brand new file) = %+v, want a-valid reported as added", added)
	}
}

func TestNewGates_MalformedOldContentTreatedAsNoGates(t *testing.T) {
	// A corrupted/unparseable prior version must not abort detection — it
	// fails toward requiring MORE evidence, never less.
	added, err := NewGates("workflows/example.yaml", []byte("not: [valid\n"), []byte(workflowOneGate))
	if err != nil {
		t.Fatalf("NewGates: %v", err)
	}
	if len(added) != 1 || added[0].Gate != "a-valid" {
		t.Fatalf("NewGates(malformed old) = %+v, want a-valid reported as added", added)
	}
}

func TestVerifyEvidence_Missing(t *testing.T) {
	newGates := []GateRef{{File: "workflows/example.yaml", Workflow: "example", Gate: "a-valid"}}
	err := VerifyEvidence(newGates, Evidence{Gates: map[string]GateEvidence{}})
	if !errors.Is(err, ErrMissingEvidence) {
		t.Fatalf("VerifyEvidence with no entry = %v, want ErrMissingEvidence", err)
	}
}

func TestVerifyEvidence_WrongVerdicts(t *testing.T) {
	newGates := []GateRef{{File: "workflows/example.yaml", Workflow: "example", Gate: "a-valid"}}
	evidence := Evidence{Gates: map[string]GateEvidence{
		"workflows/example.yaml#a-valid": {PreFix: "pass", PostFix: "pass", RunEvidence: "run-123"},
	}}
	err := VerifyEvidence(newGates, evidence)
	if !errors.Is(err, ErrNotFailFirst) {
		t.Fatalf("VerifyEvidence with preFix=pass = %v, want ErrNotFailFirst (vacuously-passing check must be rejected)", err)
	}
}

func TestVerifyEvidence_MissingProvenance(t *testing.T) {
	newGates := []GateRef{{File: "workflows/example.yaml", Workflow: "example", Gate: "a-valid"}}
	evidence := Evidence{Gates: map[string]GateEvidence{
		"workflows/example.yaml#a-valid": {PreFix: "fail", PostFix: "pass"},
	}}
	err := VerifyEvidence(newGates, evidence)
	if !errors.Is(err, ErrNotFailFirst) {
		t.Fatalf("VerifyEvidence with no runEvidence = %v, want ErrNotFailFirst", err)
	}
}

func TestVerifyEvidence_Valid(t *testing.T) {
	newGates := []GateRef{{File: "workflows/example.yaml", Workflow: "example", Gate: "a-valid"}}
	evidence := Evidence{Gates: map[string]GateEvidence{
		"workflows/example.yaml#a-valid": {PreFix: "fail", PostFix: "pass", RunEvidence: "run-123"},
	}}
	if err := VerifyEvidence(newGates, evidence); err != nil {
		t.Fatalf("VerifyEvidence(valid fail-first evidence) = %v, want nil", err)
	}
}

func TestVerifyEvidence_KeyedByFileNotGateNameAlone(t *testing.T) {
	// Two workflows adding a same-named gate each need their own entry.
	newGates := []GateRef{
		{File: "workflows/a.yaml", Workflow: "a", Gate: "shared-valid"},
		{File: "workflows/b.yaml", Workflow: "b", Gate: "shared-valid"},
	}
	evidence := Evidence{Gates: map[string]GateEvidence{
		"workflows/a.yaml#shared-valid": {PreFix: "fail", PostFix: "pass", RunEvidence: "run-1"},
	}}
	err := VerifyEvidence(newGates, evidence)
	if !errors.Is(err, ErrMissingEvidence) {
		t.Fatalf("VerifyEvidence with only one of two same-named gates covered = %v, want ErrMissingEvidence for workflows/b.yaml", err)
	}
}
