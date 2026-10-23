package childworkflow

import (
	"strings"
	"testing"
)

const humanGateProposal = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: generated-approval
spec:
  gaggle: web
  triggers: [{type: manual}]
  start: check
  tasks:
    - name: check
      type: deterministic
      goal: Run the local check
      run:
        command: ["true"]
      next: approve
  gates:
    - name: approve
      evaluator: human
      human: {}
      branches: {pass: "", reject: "@abort"}
`

// Engine parents decide their own human gates (#7760), but a generated child
// has no decision route for its family yet, so the engine backend refuses it.
func TestEngineBackendRefusesHumanGatesInGeneratedChildren(t *testing.T) {
	engineContext := testContext()
	engineContext.Backend = BackendEngine
	requireCode(t, validator(t, engineContext), humanGateProposal, "backend")
	runner := testContext()
	if _, err := validator(t, runner).Validate([]byte(humanGateProposal)); err != nil && strings.Contains(err.Error(), "occurrence-bound") {
		t.Fatalf("runner backend applied the engine-only refusal: %v", err)
	}
}
