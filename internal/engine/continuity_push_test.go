package engine

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/providers"
)

// #6902: the implementation lane's post-PR loop. implement committed (delta A),
// push-branch then published the run branch, and a later stage re-entered. The
// remote branch is the receiving ref from the push on; handing the re-entered
// pod the pre-push bundle made its ancestry guard refuse the run's own work as
// "diverged" (the guard itself is right, and is untouched).
func TestPushedBranchRetiresPrePushDeltas(t *testing.T) {
	pushFact := func(branch string) []dispatcher.SurrenderedMutation {
		return []dispatcher.SurrenderedMutation{{Provider: "git", Kind: "branch", ID: branch, Operation: "push"}}
	}
	runBranch := func(in RunInput) string {
		return providers.BranchNameIn(providers.NormalizeBranchNamespace(in.BranchNamespace), in.WorkflowName, in.RunID)
	}
	cases := []struct {
		name string
		// push builds the push stage's surrender.
		push func(in RunInput) dispatcher.SurrenderedResult
		want string
	}{
		{
			name: "push of the run branch (stage unchanged): the re-entered stage is handed no stale bundle",
			push: func(in RunInput) dispatcher.SurrenderedResult {
				return dispatcher.SurrenderedResult{
					Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, WorkspaceDeltaUnchanged: true,
					Mutations: pushFact(runBranch(in)),
				}
			},
			want: "",
		},
		{
			name: "push that also published a delta (e.g. a push-time rebase): its own bundle is the one handed on",
			push: func(in RunInput) dispatcher.SurrenderedResult {
				return dispatcher.SurrenderedResult{
					Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, WorkspaceDelta: deltaB,
					WorkspaceDeltaBase: "1111111", WorkspaceDeltaTip: "3333333",
					Mutations: pushFact(runBranch(in)),
				}
			},
			want: deltaB,
		},
		{
			name: "no push: the unpushed bundle is still handed on",
			push: func(in RunInput) dispatcher.SurrenderedResult {
				return dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, WorkspaceDeltaUnchanged: true}
			},
			want: deltaA,
		},
		{
			name: "push of some other branch (not the workspace's): the bundle is still handed on",
			push: func(in RunInput) dispatcher.SurrenderedResult {
				return dispatcher.SurrenderedResult{
					Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, WorkspaceDeltaUnchanged: true,
					Mutations: pushFact("someone/else"),
				}
			},
			want: deltaA,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "implement",
				Tasks: []apiv1.Task{
					podTask("implement", "push-branch", nil),
					podTask("push-branch", "reenter", nil),
					podTask("reenter", "", nil),
				},
			}
			in := runInput("pushed-reentry", spec)
			in.Placements = []PinnedPlacement{remotePin("implement"), remotePin("push-branch"), remotePin("reenter")}
			surrenders := surrenderStore(t)
			surrenderDelta(t, surrenders, in.RunID, "implement", 1, deltaA)
			putSurrendered(t, surrenders, in.RunID, "push-branch", 1, tc.push(in))
			putSurrendered(t, surrenders, in.RunID, "reenter", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
			fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

			executeForProjection(t, in, &Activities{Det: &fakeRunner{}, Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders}, false)
			if len(fake.attempts) != 3 {
				t.Fatalf("expected three pod attempts, got %d", len(fake.attempts))
			}
			if got := fake.attempts[1].WorkspaceDelta; got != deltaA {
				t.Fatalf("push-branch itself was handed %q, want implement's %s (the push has not happened yet)", got, deltaA)
			}
			if got := fake.attempts[2].WorkspaceDelta; got != tc.want {
				t.Fatalf("re-entered stage was handed delta %q, want %q", got, tc.want)
			}
		})
	}
}
