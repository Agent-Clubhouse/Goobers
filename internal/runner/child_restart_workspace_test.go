package runner

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestBindChildRestartWorkspaceReplacesOnlyAdmittedFork(t *testing.T) {
	run := strings.Repeat("a", 32)
	repo := &apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "owner", Name: "repo"}
	source := ChildWorkspaceAdmission{WorkspaceID: "source-child", ForkSHA: strings.Repeat("b", 40), RepositoryDigest: strings.Repeat("c", 64)}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	plan := StageRestartPlan{Source: journal.RunIdentity{Child: &journal.ChildLineage{}, WorkspaceBranch: "goobers/children/source", WorkspaceRepository: repo}, Continuation: journal.ContinuationRequest{RunID: run, ChildContinuation: &journal.ChildLineage{ExecutionEpoch: 1}, Inputs: map[string][]byte{ChildWorkspaceInputName: raw}}}
	next := ChildWorkspaceAdmission{WorkspaceID: run + "-child", ForkSHA: strings.Repeat("d", 40), RepositoryDigest: source.RepositoryDigest}
	bound, err := BindChildRestartWorkspace(plan, &next)
	if err != nil {
		t.Fatal(err)
	}
	if string(plan.Continuation.Inputs[ChildWorkspaceInputName]) != string(raw) || plan.Continuation.ChildWorkspace != nil {
		t.Fatal("source plan changed")
	}
	if bound.Continuation.ChildWorkspace.Branch != "goobers/children/"+run || bound.Continuation.ChildWorkspace.ForkSHA != next.ForkSHA || bound.Continuation.InputIntegrity[ChildWorkspaceInputName] != apiv1.IntegrityTrusted {
		t.Fatal(bound)
	}
	if _, err = BindChildRestartWorkspace(plan, nil); err == nil {
		t.Fatal("repo child lost fork custody")
	}
	next.WorkspaceID = "other-child"
	if _, err = BindChildRestartWorkspace(plan, &next); err == nil {
		t.Fatal("foreign fork accepted")
	}
}
