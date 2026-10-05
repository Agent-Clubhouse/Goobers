package workflow

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestPublicationAdmissionRequiresCurrentDSLAndCapability(t *testing.T) {
	def := Definition{Name: "publish", Version: 1, DSLVersion: "3.1", Annotations: map[string]string{"goobers.dev/allow-preview-features": "true"}, Spec: apiv1.WorkflowSpec{Gaggle: "one", Start: "send", Tasks: []apiv1.Task{{Name: "send", Type: apiv1.TaskDeterministic, Capabilities: []string{"event:publish"}, Inputs: map[string]string{"kind": "publish-event", "type": "changed", "occurrenceKey": "once"}, Run: &apiv1.DeterministicRun{Command: []string{"true"}}}}}}
	if _, err := Compile(def); err != nil {
		t.Fatal(err)
	}
	old := def
	old.DSLVersion = "3.0"
	if _, err := Compile(old); err == nil || !strings.Contains(err.Error(), "3.1") {
		t.Fatal(err)
	}
	def.Spec.Tasks[0].Capabilities = nil
	if _, err := Compile(def); err == nil || !strings.Contains(err.Error(), "event:publish") {
		t.Fatal(err)
	}
}
