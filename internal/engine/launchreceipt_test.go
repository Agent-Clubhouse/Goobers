package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestRemoteLaunchBindingRepassProjectionAndLegacyPayload(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			in := placedGateInput("launch-binding")
			in.Spec.Tasks[0] = podTask("implement", "review", nil)
			in.GooberDigest = journal.Digest([]byte("pinned kit"))
			in.Placements = []PinnedPlacement{remotePin("implement"), remoteGatePin()}
			in.MaxRepasses = 2
			d := &repassIdentityAuditDispatcher{plane: surrenderStore(t)}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			if legacy {
				env.OnGetVersion(mock.Anything, workflow.DefaultVersion, 1).Return(func(id string, _ workflow.Version, maxVersion workflow.Version) workflow.Version {
					if strings.HasPrefix(id, remoteLaunchReceiptChange+"/") {
						return workflow.DefaultVersion
					}
					return maxVersion
				})
			}
			env.RegisterActivity(&Activities{Det: &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}, Workspaces: testWorkspaces(t), Dispatcher: d, Surrenders: d.plane})
			env.ExecuteWorkflow(Run, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			value, err := env.QueryWorkflow(JournalQuery)
			if err != nil {
				t.Fatal(err)
			}
			var proj JournalProjection
			if err := value.Get(&proj); err != nil {
				t.Fatal(err)
			}
			events, _, err := projectedEvents(proj)
			if err != nil {
				t.Fatal(err)
			}
			starts := map[string]journal.Event{}
			for _, e := range events {
				if e.Type == journal.EventStageStarted || e.Type == journal.EventReviewerStarted {
					starts[journal.StageAttemptID(in.RunID, e.Branch, e.Stage, e.Seq)] = e
				}
			}
			if len(d.attempts) != 4 {
				t.Fatalf("dispatches %d", len(d.attempts))
			}
			seen := map[string]bool{}
			for _, a := range d.attempts {
				b := a.LaunchBinding
				if legacy {
					if b != nil {
						t.Fatal("old activity payload changed")
					}
					continue
				}
				if b == nil {
					t.Fatal("binding absent")
				}
				e, ok := starts[b.AttemptID]
				if !ok || seen[b.AttemptID] || e.Seq != b.StartedSeq || e.Attempt != b.Number || e.AttemptClass != b.Class || b.Review != a.Review || b.GooberDigest != in.GooberDigest || b.WorkflowDigest != proj.Identity.WorkflowDigest {
					t.Fatalf("binding differs from durable projection: %+v / %+v", b, e)
				}
				seen[b.AttemptID] = true
			}
		})
	}
}

func TestRemoteLaunchBindingSurvivesInfrastructureRetry(t *testing.T) {
	in := runInput("launch-infra", apiv1.WorkflowSpec{Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "build", Tasks: []apiv1.Task{podTask("build", "", nil)}})
	in.Placements = []PinnedPlacement{remotePin("build")}
	store := surrenderStore(t)
	putSurrendered(t, store, in.RunID, "build", 2, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	var attempts []dispatcher.Attempt
	d := placementDispatchFunc(func(_ context.Context, a dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
		attempts = append(attempts, a)
		if len(attempts) == 1 {
			return dispatcher.Report{}, fmt.Errorf("lost transport")
		}
		return dispatcher.Report{SurrenderConfirmed: true}, nil
	})
	proj := executeForProjection(t, in, &Activities{Workspaces: testWorkspaces(t), Dispatcher: d, Surrenders: store}, false)
	if len(attempts) != 2 {
		t.Fatalf("attempts=%d", len(attempts))
	}
	first, next := attempts[0].LaunchBinding, attempts[1].LaunchBinding
	if first == nil || next == nil || first.AttemptID == next.AttemptID || next.Number != 2 || next.Class != journal.AttemptInfra || first.WorkflowDigest != proj.Identity.WorkflowDigest {
		t.Fatalf("retry bindings: %+v %+v", first, next)
	}
}

type interleavingLaunchJournal struct {
	writer  *livejournal.Writer
	ordinal int
}

func (j *interleavingLaunchJournal) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	return j.writer.Emit(ctx, req)
}

func (j *interleavingLaunchJournal) EmitController(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	if req.Open == nil {
		j.ordinal++
		aux := livejournal.EmitRequest{RunID: req.RunID, Gaggle: req.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: fmt.Sprintf("pod-aux-%d", j.ordinal), Event: &journal.Event{Type: journal.EventRunnerAnnotation}}}}
		if _, err := j.writer.Emit(ctx, aux); err != nil {
			return livejournal.EmitResponse{}, err
		}
	}
	return j.writer.EmitController(ctx, req)
}

func TestRemoteLaunchBindingUsesExactLiveStartAmidAuxiliaryEvents(t *testing.T) {
	in := placedGateInput("launch-live-interleaving")
	in.Spec.Tasks[0] = podTask("implement", "review", nil)
	in.Placements = []PinnedPlacement{remotePin("implement"), remoteGatePin()}
	in.MaxRepasses = 2
	in.LiveJournal = true
	key, err := podauth.NewSignedKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	w, runsDir := newLiveWriter(t, livejournal.WithControllerStartAuthority(key))
	d := &repassIdentityAuditDispatcher{plane: surrenderStore(t)}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{Det: &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}, Workspaces: testWorkspaces(t), Dispatcher: d, Surrenders: d.plane, Journal: &interleavingLaunchJournal{writer: w}})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	starts := map[string]journal.Event{}
	for _, e := range liveEvents(t, runsDir, in.RunID) {
		if e.Type == journal.EventStageStarted || e.Type == journal.EventReviewerStarted {
			starts[journal.StageAttemptID(in.RunID, e.Branch, e.Stage, e.Seq)] = e
		}
	}
	if len(d.attempts) != 4 {
		t.Fatalf("dispatches=%d", len(d.attempts))
	}
	for _, a := range d.attempts {
		b := a.LaunchBinding
		if b == nil {
			t.Fatal("live receipt binding absent")
		}
		e, ok := starts[b.AttemptID]
		if !ok || e.Seq != b.StartedSeq {
			t.Fatalf("receipt failed canonical live identity: %+v starts=%+v", b, starts)
		}
	}
}
