package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func TestDispatchStageSelectedRevisionAttempt(t *testing.T) {
	for _, envelopeOnly := range []bool{false, true} {
		store := surrenderStore(t)
		fake := &fakeStageDispatcher{report: dispatcher.Report{
			Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true,
		}}
		putSurrendered(t, store, "selected", "inspect", 1, dispatcher.SurrenderedResult{
			Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		})
		input := dispatchInput("selected", "inspect", 1)
		input.Run = &apiv1.DeterministicRun{Command: []string{"inspect"}, Workspace: apiv1.WorkspaceRepoReadOnly}
		input.PartialClone = true
		input.Checkout = &apiv1.CheckoutSpec{Sparse: []string{"src"}}
		input.Envelope.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "base", Name: "repo"}
		selected := &apiv1.WorkspaceRevision{
			Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "fork", Name: "repo"},
			CommitSHA:  strings.Repeat("a", 40),
		}
		if envelopeOnly {
			input.Envelope.WorkspaceRevision = selected
		} else {
			input.WorkspaceRevision = selected
		}
		activities := &Activities{Dispatcher: fake, Surrenders: store}
		if _, err := activities.DispatchStage(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		attempts, _ := fake.recorded()
		if len(attempts) != 1 {
			t.Fatalf("attempts=%d", len(attempts))
		}
		got := attempts[0]
		if !reflect.DeepEqual(got.WorkspaceRevision, selected) || !reflect.DeepEqual(got.WorkspaceRepository, input.Envelope.RepoRef) || !got.PartialClone || !reflect.DeepEqual(got.Checkout, input.Checkout) {
			t.Fatalf("selected attempt lost identity, authority or policy: %+v", got)
		}
		if got.WorkspaceBranch != "" || got.WorkspaceDelta != "" || got.CheckoutCapability != "" || got.SyncBase {
			t.Fatalf("writable provisioning control leaked: %+v", got)
		}
		if got.WorkspaceRevision == selected {
			t.Fatal("attempt aliases selected workflow state")
		}
		if got.Checkout == input.Checkout || &got.Checkout.Sparse[0] == &input.Checkout.Sparse[0] {
			t.Fatal("attempt aliases checkout policy")
		}
		for _, control := range []string{"branch", "delta", "syncBase"} {
			conflicting := input
			run := *input.Run
			conflicting.Run = &run
			switch control {
			case "branch":
				conflicting.WorkspaceBranch = "same-name"
			case "delta":
				conflicting.WorkspaceDelta = "sha256:delta"
			case "syncBase":
				conflicting.Run.SyncBase = true
			}
			if _, err := activities.DispatchStage(context.Background(), conflicting); err == nil || !strings.Contains(err.Error(), workspacerevision.CodeConflict) {
				t.Fatalf("selected %s override error=%v", control, err)
			}
		}
		if attempts, _ := fake.recorded(); len(attempts) != 1 {
			t.Fatalf("conflicting controls reached dispatcher: %d attempts", len(attempts))
		}
	}
}
