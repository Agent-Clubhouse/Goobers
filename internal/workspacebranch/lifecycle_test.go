package workspacebranch

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestOwnedBranchSourceCollisionAndTipAuthority(t *testing.T) {
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	revision := &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{Provider: base.Provider, Owner: base.Owner, Name: base.Name, ID: "123"},
		CommitSHA:  strings.Repeat("a", 40), SourceRef: "refs/heads/ns/workflow/run",
	}
	if _, err := Expected(base, revision, "ns/", "workflow", "run"); err == nil {
		t.Fatal("source collision accepted")
	}
	revision.SourceRef = "topic"
	binding, err := Expected(base, revision, "ns/", "workflow", "run")
	if err != nil {
		t.Fatal(err)
	}
	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceBranchTip: strings.Repeat("b", 40)}
	for _, producer := range []string{KindPublish, KindEstablish, "shell"} {
		task := apiv1.Task{Type: apiv1.TaskDeterministic, Inputs: map[string]string{"kind": producer}}
		_, err := ValidateResult(binding, revision, base, "ns/", "workflow", "run", task, result)
		if (err == nil) != (producer == KindPublish) {
			t.Fatalf("producer %s: %v", producer, err)
		}
		task.Type = apiv1.TaskAgentic
		if _, err := ValidateResult(binding, revision, base, "ns/", "workflow", "run", task, result); err == nil {
			t.Fatal("agent tip accepted")
		}
	}
	for _, raw := range []string{`null`, `"abc"`, `false`, `{"sha":"abc"}`} {
		if _, err := ResultTip([]byte(`{"workspaceBranchTip":` + raw + `}`)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
