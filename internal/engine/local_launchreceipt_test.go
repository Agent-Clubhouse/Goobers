package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestLocalLaunchBindingsSurviveRetryRepassProjectionAndLegacyHistory(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%t/live=%t", legacy, live), func(t *testing.T) {
				in := placedGateInput("local-launch-binding")
				in.LiveJournal = live
				in.GooberDigest = journal.Digest([]byte("pinned goober"))
				in.Spec.Gates[0].Agentic.Retry = &apiv1.RetryPolicy{MaxAttempts: 2}
				authority, err := podauth.NewLocalStartAuthority()
				if err != nil {
					t.Fatal(err)
				}
				writer, runsDir := newLiveWriter(t, livejournal.WithControllerStartAuthority(authority))
				var bindings []launchreceipt.Binding
				recorder := launchreceipt.LocalRecorder{Root: t.TempDir()}
				capture := func(ctx context.Context, env apiv1.InvocationEnvelope, review bool) error {
					binding, ok := launchreceipt.ContextBinding(ctx)
					if legacy {
						if ok {
							return errors.New("legacy activity acquired a new binding")
						}
						return nil
					}
					if !ok {
						return errors.New("new local activity lost its binding")
					}
					bindings = append(bindings, binding)
					return launchreceipt.RecordLocalInvocation(ctx, recorder, env, review, launchreceipt.PreparedLocal("agent"))
				}
				reviews := 0
				invoker := &fakeInvoker{invoke: func(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, capture(ctx, env, false)
				}, review: func(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
					if err := capture(ctx, env, true); err != nil {
						return apiv1.Verdict{}, err
					}
					reviews++
					if reviews == 1 {
						return apiv1.Verdict{}, invoke.InfrastructureFailure(errors.New("fixture retry"))
					}
					if reviews == 2 {
						return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges}, nil
					}
					return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
				}}
				det := &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, capture(ctx, env, false)
				}}
				var suite testsuite.WorkflowTestSuite
				env := temporaltest.NewWorkflowEnvironment(&suite)
				if legacy {
					env.OnGetVersion(mock.Anything, workflow.DefaultVersion, 1).Return(func(id string, _ workflow.Version, max workflow.Version) workflow.Version {
						if strings.HasPrefix(id, localLaunchReceiptChange+"/") {
							return workflow.DefaultVersion
						}
						return max
					})
				}
				env.RegisterActivity(&Activities{Goober: invoker, Det: det, Workspaces: testWorkspaces(t), Journal: writer})
				env.ExecuteWorkflow(Run, in)
				if err := env.GetWorkflowError(); err != nil {
					t.Fatal(err)
				}
				if legacy {
					return
				}
				value, err := env.QueryWorkflow(JournalQuery)
				if err != nil {
					t.Fatal(err)
				}
				var projection JournalProjection
				if err := value.Get(&projection); err != nil {
					t.Fatal(err)
				}
				if len(bindings) != 6 {
					t.Fatalf("bindings=%d; want producer/reviewer retries and repass plus deterministic", len(bindings))
				}
				for range 2 {
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
					if live {
						events = liveEvents(t, runsDir, in.RunID)
					}
					starts := map[string]journal.Event{}
					for _, event := range events {
						if event.Type == journal.EventStageStarted || event.Type == journal.EventReviewerStarted {
							starts[journal.StageAttemptID(in.RunID, event.Branch, event.Stage, event.Seq)] = event
						}
					}
					seen := map[string]bool{}
					for _, binding := range bindings {
						event, ok := starts[binding.AttemptID]
						if !ok || seen[binding.AttemptID] || binding.StartedSeq != event.Seq || binding.Number != event.Attempt || binding.Class != event.AttemptClass || binding.GooberDigest != in.GooberDigest || binding.WorkflowDigest != projection.Identity.WorkflowDigest {
							t.Fatalf("identity drift: %+v / %+v", binding, event)
						}
						seen[binding.AttemptID] = true
					}
				}
			})
		}
	}
}
