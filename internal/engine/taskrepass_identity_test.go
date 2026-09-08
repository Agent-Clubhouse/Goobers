package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

type repassIdentityAuditDispatcher struct {
	plane     *dispatcher.SurrenderDir
	attempts  []dispatcher.Attempt
	taskCalls int
}

func (d *repassIdentityAuditDispatcher) Dispatch(ctx context.Context, attempt dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	d.attempts = append(d.attempts, attempt)
	surrendered := dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}
	if attempt.Stage == "implement" {
		d.taskCalls++
		revision := fmt.Sprintf("implementation-%d", d.taskCalls)
		surrendered.Result.Outputs = map[string]any{"revision": revision}
		ref, err := journal.ArtifactRef([]byte(revision))
		if err != nil {
			return dispatcher.Report{}, err
		}
		surrendered.Result.Artifacts = []apiv1.ArtifactPointer{{Digest: ref.Digest, Path: ref.Path, Size: ref.Size, Integrity: apiv1.IntegrityDerived}}
	} else {
		decision := apiv1.VerdictNeedsChanges
		if attempt.Number > 1 {
			decision = apiv1.VerdictPass
		}
		surrendered = reviewSurrender(apiv1.Verdict{Decision: decision, Summary: "review"})
	}
	data, err := json.Marshal(surrendered)
	if err != nil {
		return dispatcher.Report{}, err
	}
	// The production write-once surrender store receives each new pod's result.
	if err := d.plane.Put(ctx, attempt.RunID, attempt.Stage, attempt.IdentityAttempt(), data); err != nil {
		return dispatcher.Report{}, err
	}
	confirmed, err := (dispatcher.PlaneSurrenderGate{Plane: d.plane}).Confirmed(ctx, attempt)
	return dispatcher.Report{Runner: "linux-agentic", Pod: dispatcher.PodName(attempt), Phase: corev1.PodSucceeded, SurrenderConfirmed: confirmed}, err
}

func TestRemoteTaskRepassRequiresFreshSurrenderIdentity(t *testing.T) {
	for _, legacyPrefix := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("legacy-prefix=%d", legacyPrefix), func(t *testing.T) {
			legacy := legacyPrefix == 2
			in := placedGateInput("task-repass-identity")
			in.Spec.Tasks[0] = podTask("implement", "review", nil)
			in.Placements = []PinnedPlacement{remotePin("implement"), remoteGatePin()}
			in.MaxRepasses = 2
			d := &repassIdentityAuditDispatcher{plane: surrenderStore(t)}
			det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			for _, stage := range []string{"implement", "review"} {
				for ordinal := 1; ordinal <= legacyPrefix; ordinal++ {
					env.OnGetVersion(dispatchPodAttemptChangeID(stage, ordinal), workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
				}
			}
			env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t), Dispatcher: d, Surrenders: d.plane})
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
			var identities, revisions, digests []string
			for _, a := range d.attempts {
				if a.Stage == "implement" {
					identities = append(identities, fmt.Sprintf("%s/%s/%d (%s)", a.RunID, a.Stage, a.IdentityAttempt(), dispatcher.PodName(a)))
				}
			}
			for _, op := range proj.Ops {
				if e := op.Event; e != nil && e.Type == journal.EventStageFinished && e.Stage == "implement" {
					revisions = append(revisions, fmt.Sprint(e.Outputs["revision"]))
					if len(e.Artifacts) != 1 {
						t.Fatalf("missing surrendered artifact: %+v", e)
					}
					digests = append(digests, e.Artifacts[0].Digest)
					if e.Attempt != 1 || e.AttemptClass != "" {
						t.Fatalf("journal graph-visit lineage changed: %+v", e)
					}
				}
			}
			t.Logf("task dispatches=%d identities=%v observed revisions=%v", d.taskCalls, identities, revisions)
			if len(digests) != 2 || (!legacy && digests[0] == digests[1]) {
				t.Fatalf("repass did not consume new artifact: %v", digests)
			}
			wantSecond := "implementation-2"
			if legacy {
				wantSecond = "implementation-1"
			}
			if len(identities) != 2 || (!legacy && identities[0] == identities[1]) {
				t.Fatalf("physical identities=%v", identities)
			}
			if d.taskCalls != 2 || len(revisions) != 2 || revisions[0] != "implementation-1" || revisions[1] != wantSecond {
				t.Fatalf("repass consumed old surrender: identities=%v outputs=%v; want distinct identity and implementation-2", identities, revisions)
			}

		})
	}
}
