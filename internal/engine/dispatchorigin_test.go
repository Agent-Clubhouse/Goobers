package engine

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
)

func TestDispatchStagePreservesDeclaredForgeOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  *apiv1.DeterministicRun
	}{
		{"CLI", &apiv1.DeterministicRun{Command: []string{"goobers", "open-pr"}, Workspace: apiv1.WorkspaceScratch}},
		{"repo shell", &apiv1.DeterministicRun{Command: []string{"make", "ci"}, Workspace: apiv1.WorkspaceRepo}},
		{"agentic repo", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, origin := range []string{"https://forge.example/root/$(GOOBERS_POD_TOKEN)/", ""} {
				fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
				store := surrenderStore(t)
				putSurrendered(t, store, "origin", "task", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
				a := &Activities{Dispatcher: fake, Surrenders: store}
				in := dispatchInput("origin", "task", 1)
				in.Run = tc.run
				in.Workspace = apiv1.WorkspaceRepo
				in.Envelope.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderGitea, BaseURL: origin, Owner: "acme", Name: "repo"}
				if _, err := a.DispatchStage(context.Background(), in); err != nil {
					t.Fatal(err)
				}
				got, present := fake.attempts[0].RunContext[executor.RepoBaseURLEnvVar]
				if !present || got != origin {
					t.Fatalf("origin stamp = %q (present=%t), want explicit %q", got, present, origin)
				}
			}
		})
	}
}
