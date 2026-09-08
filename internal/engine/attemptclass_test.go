package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

type lineageDispatcher struct {
	*fakeStageDispatcher
	failures []error
}

func (d *lineageDispatcher) Dispatch(ctx context.Context, attempt dispatcher.Attempt, eligible []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	report, err := d.fakeStageDispatcher.Dispatch(ctx, attempt, eligible)
	if attempt.Number <= len(d.failures) {
		return dispatcher.Report{}, d.failures[attempt.Number-1]
	}
	return report, err
}

func TestRemoteTaskRetryTransportsAttemptLineage(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy-history"
		}
		t.Run(name, func(t *testing.T) {
			in := runInput("lineage-task", remotelyPlacedSpec("build"))
			in.Spec.Tasks[0].Retry = &apiv1.RetryPolicy{MaxAttempts: 2}
			in.Placements = remotePlacement("build")
			store := surrenderStore(t)
			putSurrendered(t, store, in.RunID, "build", 3, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
			d := &lineageDispatcher{succeedingStageDispatcher(), []error{
				dispatcher.ErrSurrenderUnconfirmed,
				&dispatcher.SelectionError{Diagnostic: "policy refusal"},
			}}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			if legacy {
				env.OnGetVersion(dispatchAttemptClassChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: d, Surrenders: store})
			env.ExecuteWorkflow(Run, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			attempts, _ := d.recorded()
			want := []journal.AttemptClass{"", journal.AttemptInfra, journal.AttemptPolicy}
			if legacy {
				want = []journal.AttemptClass{"", "", ""}
			}
			assertDispatchedClasses(t, attempts, want)
		})
	}
}

func TestDispatchOnePreservesSuppliedAndLegacyAttemptLineage(t *testing.T) {
	for _, class := range []journal.AttemptClass{"", journal.AttemptPolicy, journal.AttemptInfra, journal.AttemptHuman} {
		name := string(class)
		if name == "" {
			name = "legacy-omitted"
		}
		t.Run(name, func(t *testing.T) {
			in := dispatchInput("lineage-one", "build", 2)
			in.Class = class
			in.Run = &apiv1.DeterministicRun{Command: []string{"build"}, Workspace: apiv1.WorkspaceScratch}
			in.Placement.LedgerTouching = false
			data, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			_, present := fields["class"]
			if present != (class != "") {
				t.Fatalf("class presence = %t for %q", present, class)
			}
			var decoded DispatchStageInput
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			store := surrenderStore(t)
			putSurrendered(t, store, in.Envelope.RunID, "build", 2, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
			d := succeedingStageDispatcher()
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: DispatchOneWorkflowID(in.Envelope.RunID, "build", 2)})
			env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: d, Surrenders: store})
			env.ExecuteWorkflow(DispatchOne, decoded)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			attempts, _ := d.recorded()
			if len(attempts) != 1 || attempts[0].Number != 2 || attempts[0].Class != class {
				t.Fatalf("dispatched attempts = %+v", attempts)
			}
		})
	}
}

func TestRemoteReviewerRetryAndRepassTransportAttemptLineage(t *testing.T) {
	in := placedGateInput("lineage-review")
	in.Placements = []PinnedPlacement{remoteGatePin()}
	in.MaxRepasses = 2
	in.Spec.Gates[0].Agentic.Retry = &apiv1.RetryPolicy{MaxAttempts: 2}
	store := surrenderStore(t)
	putSurrendered(t, store, in.RunID, "review", 2, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "retry the subject"}))
	putSurrendered(t, store, in.RunID, "review", 3, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "done"}))
	d := &lineageDispatcher{succeedingStageDispatcher(), []error{dispatcher.ErrSurrenderUnconfirmed}}
	// Gate placement is Linux; the fake's reported runner must name that pin.
	d.report.Runner = "linux-agentic"
	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}
	executeForProjection(t, in, &Activities{Goober: refusingReviewer(t), Det: det, Workspaces: testWorkspaces(t), Dispatcher: d, Surrenders: store}, false)
	attempts, _ := d.recorded()
	assertDispatchedClasses(t, attempts, []journal.AttemptClass{"", journal.AttemptInfra, journal.AttemptPolicy})
}

func assertDispatchedClasses(t *testing.T, attempts []dispatcher.Attempt, want []journal.AttemptClass) {
	t.Helper()
	var got []journal.AttemptClass
	for i, attempt := range attempts {
		if attempt.Number != i+1 {
			t.Fatalf("attempt number = %d at dispatch %d", attempt.Number, i+1)
		}
		got = append(got, attempt.Class)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatched lineage = %q, want %q", got, want)
	}
}
