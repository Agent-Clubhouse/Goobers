package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestMaterializeAgenticKitGooberUsesPinnedWorkflowMCPDefaults(t *testing.T) {
	set := &instance.ConfigSet{Workflows: []apiv1.Workflow{
		{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}, DSLVersion: "3.0", Spec: apiv1.WorkflowSpec{Gaggle: "web"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "current"}, DSLVersion: "3.1", Spec: apiv1.WorkflowSpec{Gaggle: "web"}},
	}}
	spec := apiv1.GooberSpec{MCPServers: []apiv1.MCPServer{{Name: "context", Command: "context-server"}}}
	for _, tc := range []struct {
		workflow     string
		wantRequired bool
	}{
		{workflow: "legacy"},
		{workflow: "current", wantRequired: true},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			got := materializeAgenticKitGoober(set, apiv1.InvocationEnvelope{
				WorkflowID: tc.workflow, Gaggle: "web",
			}, spec)
			required := got.MCPServers[0].Required
			if tc.wantRequired {
				if required == nil || !*required {
					t.Fatalf("required = %v, want true", required)
				}
			} else if required != nil {
				t.Fatalf("required = %v, want legacy nil", *required)
			}
		})
	}
	if spec.MCPServers[0].Required != nil {
		t.Fatal("kit materialization mutated shared goober config")
	}
}
