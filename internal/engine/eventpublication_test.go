package engine

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestEnginePublicationRequiresQualifiedTransport(t *testing.T) {
	spec := apiv1.WorkflowSpec{Tasks: []apiv1.Task{{Name: "publish", Type: apiv1.TaskDeterministic, Inputs: map[string]string{"kind": "publish-event"}}}}
	if err := RefuseDefinition("producer", spec); err == nil || !strings.Contains(err.Error(), "publish-event requires the local daemon runner") {
		t.Fatal(err)
	}
}
