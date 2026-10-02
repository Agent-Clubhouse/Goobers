package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestReviewerLifecycleVersionedRetryAndRepass(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			in := placedGateInput("reviewer-lifecycle")
			in.Placements = nil
			in.LiveJournal = true
			writer, runsDir := newLiveWriter(t)
			in.Spec.Gates[0].Agentic.Retry = &apiv1.RetryPolicy{MaxAttempts: 2}
			var numbers []int32
			inv := &fakeInvoker{invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}, review: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
				if !legacy {
					var latest journal.Event
					for _, event := range liveEvents(t, runsDir, in.RunID) {
						if event.Type == journal.EventReviewerStarted {
							latest = event
						}
					}
					if latest.Attempt != int(env.Attempt) {
						t.Errorf("review before its durable start: %+v env=%d", latest, env.Attempt)
					}
				}
				numbers = append(numbers, env.Attempt)
				switch len(numbers) {
				case 1:
					return apiv1.Verdict{}, invoke.InfrastructureFailure(errors.New("review connection lost"))
				case 2:
					return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges}, nil
				default:
					return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
				}
			}}
			det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			if legacy {
				env.OnGetVersion(reviewerAttemptLifecycleChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			env.RegisterActivity(&Activities{Goober: inv, Det: det, Workspaces: testWorkspaces(t), Journal: writer})
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
			var starts, ends []journal.Event
			for _, op := range projection.Ops {
				if op.Event == nil {
					continue
				}
				if op.Event.Type == journal.EventReviewerStarted {
					starts = append(starts, *op.Event)
				}
				if op.Event.Type == journal.EventReviewerFinished {
					ends = append(ends, *op.Event)
				}
			}
			if legacy {
				if len(starts) != 0 || len(ends) != 0 || !reflect.DeepEqual(numbers, []int32{0, 0, 0}) {
					t.Fatalf("old history changed: %v %+v %+v", numbers, starts, ends)
				}
				return
			}
			if !reflect.DeepEqual(numbers, []int32{1, 2, 1}) || len(starts) != 3 || len(ends) != 3 {
				t.Fatalf("dispatches=%v starts=%+v ends=%+v", numbers, starts, ends)
			}
			classes := []journal.AttemptClass{"", journal.AttemptInfra, ""}
			for i := range starts {
				if starts[i].AttemptClass != classes[i] || ends[i].AttemptClass != classes[i] {
					t.Fatalf("lineage=%+v %+v", starts, ends)
				}
			}
			if ends[0].Status != "failure" || ends[0].Runner["retryFailureClass"] != "infra" || ends[1].Verdict != "needs-changes" {
				t.Fatalf("outcomes=%+v", ends)
			}
			// Repair projection and repeated reads must preserve the same durable
			// start anchors, including the failed attempt and the repass at number 1.
			var identities []string
			for pass := 0; pass < 2; pass++ {
				dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), projection)
				if err != nil {
					t.Fatal(err)
				}
				reader, err := journal.OpenRead(dir)
				if err != nil {
					t.Fatal(err)
				}
				events, err := reader.Events()
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				for _, event := range events {
					if event.Type == journal.EventReviewerStarted {
						ids = append(ids, journal.StageAttemptID(in.RunID, event.Branch, event.Stage, event.Seq))
					}
				}
				if pass == 0 {
					identities = ids
				} else if !reflect.DeepEqual(ids, identities) {
					t.Fatalf("projection drift: %v vs %v", ids, identities)
				}
			}
			live := liveEvents(t, runsDir, in.RunID)
			if divergence, err := DiffLiveJournal(live, projection); err != nil || divergence != "" {
				t.Fatalf("live/repair divergence=%q err=%v", divergence, err)
			}
			var liveIDs []string
			for _, event := range live {
				if event.Type == journal.EventReviewerStarted {
					liveIDs = append(liveIDs, journal.StageAttemptID(in.RunID, event.Branch, event.Stage, event.Seq))
				}
			}
			if !reflect.DeepEqual(liveIDs, identities) {
				t.Fatalf("live/repair identity drift: %v vs %v", liveIDs, identities)
			}
			if len(identities) != 3 || identities[0] == identities[1] || identities[1] == identities[2] {
				t.Fatalf("non-distinct identities=%v", identities)
			}
		})
	}
}
