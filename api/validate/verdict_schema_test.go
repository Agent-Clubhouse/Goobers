package validate

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestOrdinaryGateVerdictValidatesAgainstSchema proves an in-run gate
// Verdict — no PR-lifecycle fields at all — still validates against
// verdict.schema.json after #358's additions (headSha/baseSha/class are all
// optional, so their absence must not break the existing contract).
func TestOrdinaryGateVerdictValidatesAgainstSchema(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{Severity: apiv1.SeverityError, Message: "not concurrency safe"}},
	}
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := v.ValidateJSON("verdict.schema.json", data); err != nil {
		t.Fatalf("ordinary gate verdict should validate, got: %v", err)
	}
}

// TestPRLifecycleVerdictValidatesAgainstSchema is issue #358's schema-side
// acceptance: a real Go-marshaled merge-review Verdict — SHA-pinned, with
// classed findings — validates against verdict.schema.json, not just the Go
// struct's own round-trip.
func TestPRLifecycleVerdictValidatesAgainstSchema(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{
			{Severity: apiv1.SeverityWarning, Message: "base advanced", Class: apiv1.FindingRebaseNeeded},
			{Severity: apiv1.SeverityError, Message: "cross-PR overlap", Location: "internal/runner/run.go", Class: apiv1.FindingSubstantive},
		},
		HeadSHA: "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
		BaseSHA: "0123456789abcdef0123456789abcdef01234567",
	}
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := v.ValidateJSON("verdict.schema.json", data); err != nil {
		t.Fatalf("PR-lifecycle verdict should validate, got: %v", err)
	}
}

// TestVerdictSchemaRejectsUnknownFindingClass proves the schema's finding
// class enum is closed (fail closed on an unrecognized class, the same
// discipline the rest of this DSL's enums use).
func TestVerdictSchemaRejectsUnknownFindingClass(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	doc := `{"decision":"needs-changes","findings":[{"severity":"error","message":"x","class":"bogus-class"}]}`
	if err := v.ValidateJSON("verdict.schema.json", []byte(doc)); err == nil {
		t.Fatal("expected an unknown finding class to fail schema validation")
	}
}
