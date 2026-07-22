package validate

import "testing"

func TestInvocationInstructionAddendumValidatesAgainstSchema(t *testing.T) {
	validator, err := New()
	if err != nil {
		t.Fatal(err)
	}
	envelope := []byte(`{
		"taskId":"implement","workflowId":"implementation","runId":"run-1","gaggle":"goobers",
		"goal":"implement the issue","instructionAddendum":"Reuse the existing parser.",
		"workspace":"/workspace",
		"repoRef":{"provider":"github","owner":"acme","name":"web"},
		"limits":{}
	}`)
	if err := validator.ValidateJSON("invocation.schema.json", envelope); err != nil {
		t.Fatalf("invocation with one-off instruction addendum should validate: %v", err)
	}
}
