package engine

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestDispatchOneTransportsSelectedRevisionAndLegacyPayloads(t *testing.T) {
	for _, kind := range []string{"deterministic", "agentic", "review"} {
		for _, control := range []string{"legacy", "input", "envelope", "both"} {
			t.Run(kind+"/"+control, func(t *testing.T) {
				in := dispatchInput("runner-selected", "inspect", 1)
				in.Placement.LedgerTouching = false
				in.Envelope.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "base", Name: "repo", Branch: "main"}
				in.Workspace = apiv1.WorkspaceRepoReadOnly
				if kind == "deterministic" {
					in.Run = &apiv1.DeterministicRun{Command: []string{"inspect"}, Workspace: apiv1.WorkspaceRepoReadOnly}
				} else {
					in.Envelope.Goober = "reviewer"
					in.Review = kind == "review"
				}
				selected := selectedRevisionFixture()
				selected.Repository.Owner = "fork"
				if control != "legacy" {
					in.PartialClone = true
					in.Checkout = &apiv1.CheckoutSpec{Sparse: []string{"source", "tests"}}
					if control == "input" || control == "both" {
						in.WorkspaceRevision = selected.DeepCopy()
					}
					if control == "envelope" || control == "both" {
						in.Envelope.WorkspaceRevision = selected.DeepCopy()
					}
				}
				data, err := json.Marshal(in)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				for field, want := range map[string]bool{
					"workspaceRevision": control == "input" || control == "both",
					"checkout":          control != "legacy", "partialClone": control != "legacy",
				} {
					if _, present := fields[field]; present != want {
						t.Fatalf("%s presence = %v, want %v", field, present, want)
					}
				}
				var decoded DispatchStageInput
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				store := surrenderStore(t)
				surrender := dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}
				if in.Review {
					surrender = reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "inspected"})
				}
				putSurrendered(t, store, in.Envelope.RunID, "inspect", 1, surrender)
				d := succeedingStageDispatcher()
				var suite testsuite.WorkflowTestSuite
				env := temporaltest.NewWorkflowEnvironment(&suite)
				workflowID := DispatchOneWorkflowID(in.Envelope.RunID, "inspect", 1)
				env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: workflowID})
				env.RegisterActivity(&Activities{Dispatcher: d, Surrenders: store})
				env.ExecuteWorkflow(DispatchOne, decoded)
				if err := env.GetWorkflowError(); err != nil {
					t.Fatal(err)
				}
				attempts, _ := d.recorded()
				if len(attempts) != 1 {
					t.Fatalf("dispatch attempts = %d, want 1", len(attempts))
				}
				got := attempts[0]
				if !reflect.DeepEqual(got.WorkspaceRepository, in.Envelope.RepoRef) ||
					!reflect.DeepEqual(got.Checkout, in.Checkout) || got.PartialClone != in.PartialClone {
					t.Fatalf("base authority or pinned materialization changed: %+v", got)
				}
				if control == "legacy" {
					if got.WorkspaceRevision != nil {
						t.Fatal("legacy input invented a selected revision")
					}
				} else {
					if !reflect.DeepEqual(got.WorkspaceRevision, selected) {
						t.Fatalf("selected revision changed: %+v", got.WorkspaceRevision)
					}
					if got.CheckoutCapability != "" {
						t.Fatalf("selected revision acquired legacy checkout authority: %q", got.CheckoutCapability)
					}
				}
				if got.WorkspaceBranch != "" || got.WorkspaceDelta != "" || got.SyncBase {
					t.Fatalf("readonly dispatch mixed writable continuity: %+v", got)
				}
				if got.OwningWorkflowID != workflowID || got.Agentic != (kind != "deterministic") || got.Review != in.Review {
					t.Fatalf("dispatch identity or kind changed: %+v", got)
				}
			})
		}
	}
}
