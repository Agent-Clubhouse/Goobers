package engine

import (
	"context"

	"testing"

	"strings"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/temporaltest"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func reviewerScopeFixture(t *testing.T) (RunInput, *Activities) {
	t.Helper()
	input, activities, _, _, _, _, _ := reviewerPreparationFixture(t)
	input.Spec.Gates[0].Branches["pass"] = "after"
	input.Spec.Tasks = append(input.Spec.Tasks, apiv1.Task{Name: "after", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "consume public upstream"})
	return input, activities
}

func TestReviewerDiffIsPrivateToReviewer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version workflow.Version
		private bool
	}{{"current", 1, false}, {"legacy", workflow.DefaultVersion, true}} {
		t.Run(tc.name, func(t *testing.T) {
			input, activities := reviewerScopeFixture(t)
			invoker := activities.Goober.(*fakeInvoker)
			downstream := make(chan apiv1.InvocationEnvelope, 1)
			invoker.invoke = func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				if strings.HasSuffix(env.TaskID, ":after") {
					downstream <- env
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(activities)
			env.OnGetVersion(reviewerEvidenceScopeChange, workflow.DefaultVersion, 1).Return(tc.version)
			env.ExecuteWorkflow(Run, input)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			select {
			case invocation := <-downstream:
				private, verdict := false, false
				for _, pointer := range invocation.ContextPointers {
					private = private || pointer.Name == runsemantics.ReviewerDiffPointerName("review")
					verdict = verdict || pointer.Name == "review.verdict"
				}
				if private != tc.private || !verdict {
					t.Fatalf("downstream private=%v verdict=%v want private=%v and public verdict", private, verdict, tc.private)
				}
			default:
				t.Fatal("downstream task never executed")
			}
		})
	}
}

func TestReviewerScopeCapturedNMinusOneReplays(t *testing.T) {
	// Captured from main 3f7296e2c, with an actual task after the reviewer.
	history := loadHistoryFixture(t, "testdata/reviewer-scope-n-minus-one.json")
	leaked := false
	for _, event := range history.Events {
		if marker := event.GetMarkerRecordedEventAttributes(); marker != nil && strings.Contains(marker.String(), reviewerEvidenceScopeChange) {
			t.Fatal("fixture is not N-1")
		}
		if scheduled := event.GetActivityTaskScheduledEventAttributes(); scheduled != nil {
			for _, payload := range scheduled.Input.GetPayloads() {
				data := string(payload.Data)
				leaked = leaked || (strings.Contains(data, ":after\"") && strings.Contains(data, runsemantics.ReviewerDiffPointerName("review")))
			}
		}
	}
	if !leaked {
		t.Fatal("fixture does not exercise the old downstream leak")
	}
	replayEveryWorkflowTask(t, history, replayReviewerPreparation)
}
