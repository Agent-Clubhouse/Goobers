package engine

import (
	"context"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestTemporalReviewerDeferralCapabilityFollowsDeclaredBranch(t *testing.T) {
	for _, declared := range []bool{false, true} {
		in := runInput("review-deferral", gatedSpec())
		in.DSLVersion = "3.0"
		if declared {
			in.Spec.Gates[0].Branches["defer"] = ""
		}
		inv := successInvoker()
		called := false
		inv.review = func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			called = true
			if env.ReviewerDeferralAllowed != declared {
				t.Errorf("declared deferral=%v, reviewer capability=%v", declared, env.ReviewerDeferralAllowed)
			}
			if declared {
				return apiv1.Verdict{Decision: apiv1.VerdictDefer, ReasonCode: apiv1.VerdictReasonOrdering, Rationale: "Wait for a sibling."}, nil
			}
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		}
		var suite testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&suite)
		env.RegisterActivity(&Activities{Goober: inv, Workspaces: testWorkspaces(t)})
		env.ExecuteWorkflow(Run, in)
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		if !called {
			t.Fatal("reviewer was never dispatched")
		}
	}
}
