package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestRemoteReviewerJournalsFailedAndSettledPodPlacement(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			in := placedGateInput("review-placement")
			in.Placements = []PinnedPlacement{remoteGatePin()}
			in.Spec.Gates[0].Agentic.Retry = &apiv1.RetryPolicy{MaxAttempts: 2}
			store := surrenderStore(t)
			putSurrendered(t, store, in.RunID, "review", 2, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "reviewed"}))
			d := placementDispatchFunc(func(_ context.Context, attempt dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
				report := dispatcher.Report{Runner: "actual-reviewer", Pod: fmt.Sprintf("review-pod-%d", attempt.Number), Image: "observed-image", Node: "observed-node", OS: "linux", QueuedAt: time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)}
				if attempt.Number == 1 {
					return report, errors.New("review pod lost")
				}
				report.Phase, report.SurrenderConfirmed = corev1.PodSucceeded, true
				return report, nil
			})
			det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			if legacy {
				env.OnGetVersion(gatePlacementChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: d, Surrenders: store, Goober: refusingReviewer(t), Det: det})
			env.ExecuteWorkflow(Run, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			value, err := env.QueryWorkflow(JournalQuery)
			if err != nil {
				t.Fatal(err)
			}
			var projection JournalProjection
			if err := value.Get(&projection); err != nil {
				t.Fatal(err)
			}
			placements := projectedPlacements(t, projection)
			for attempt := 1; attempt <= 2; attempt++ {
				got, exists := placements[fmt.Sprintf("review#%d", attempt)]
				if legacy {
					if exists {
						t.Fatal("legacy history acquired a new reviewer placement event")
					}
					continue
				}
				if !exists || got.Pod != fmt.Sprintf("review-pod-%d", attempt) || got.Node != "observed-node" || got.OS != "linux" || got.Runner != "actual-reviewer" {
					t.Fatalf("attempt %d placement = %+v, present=%t", attempt, got, exists)
				}
			}
			for _, op := range projection.Ops {
				if op.Event == nil || op.Event.Type != journal.EventRunnerPlacement || op.Event.Stage != "review" {
					continue
				}
				want := journal.AttemptClass("")
				if op.Event.Attempt == 2 {
					want = journal.AttemptInfra
				}
				if op.Event.AttemptClass != want {
					t.Fatalf("placement changed retry lineage: %+v", op.Event)
				}
			}
		})
	}
}
