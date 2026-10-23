package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildExecutionBindingComesFromTheParentIdentity(t *testing.T) {
	pinned := childworkflow.AdmissionContext{Backend: childworkflow.BackendEngine, Gaggle: apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}, BranchNamespace: "bots"}}}
	workspace := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "feature"}
	parent := journal.RunIdentity{InstanceID: "instance-a", WorkspaceRepository: &workspace}
	got := childExecutionBinding(parent, pinned)
	if got == nil || got.InstanceID != "instance-a" || got.RepoRef != workspace || got.BranchNamespace == "" {
		t.Fatalf("engine binding = %+v", got)
	}
	parent.WorkspaceRepository = nil
	if got := childExecutionBinding(parent, pinned); got == nil || got.RepoRef != pinned.Gaggle.Spec.Project {
		t.Fatalf("binding without a workspace repository = %+v", got)
	}
	pinned.Backend = childworkflow.BackendRunner
	if got := childExecutionBinding(parent, pinned); got != nil {
		t.Fatalf("runner child carried an execution binding: %+v", got)
	}
}
