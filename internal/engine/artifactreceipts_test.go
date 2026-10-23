package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func fixtureArtifactReceipt(t *testing.T) invoke.ArtifactReceipt {
	t.Helper()
	ref, err := journal.ArtifactRef([]byte("artifact bytes"))
	if err != nil {
		t.Fatal(err)
	}
	return invoke.ArtifactReceipt{Name: "result.json", Ref: apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, Integrity: ref.Integrity}}
}

func TestArtifactReceiptsSurviveFailureEncodingAndKeepRetryEvidence(t *testing.T) {
	receipt := fixtureArtifactReceipt(t)
	retryAt := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	cause := classifySeamError(invoke.InfrastructureFailureUntil(errors.New("worker failed after recording"), retryAt))
	cause = withDispatchFailureEvidence(cause, dispatcher.Report{Runner: "worker", Pod: "pod"}, &AttemptUsage{Metrics: map[string]float64{"tokens": 5}}, false)
	err := withArtifactReceipts(cause, []invoke.ArtifactReceipt{receipt})
	converter := temporal.GetDefaultFailureConverter()
	decoded := converter.FailureToError(converter.ErrorToFailure(err))
	if got := failedArtifactReceipts(decoded); !reflect.DeepEqual(got, []invoke.ArtifactReceipt{receipt}) {
		t.Fatalf("receipts after encoding = %+v", got)
	}
	if got := DispatchFailurePlacement(decoded); got == nil || got.Runner != "worker" {
		t.Fatalf("lost placement %+v", got)
	}
	if got := dispatchFailureUsage(decoded); got.Metrics["tokens"] != 5 {
		t.Fatalf("lost usage %+v", got)
	}
	if got := infrastructureRetryDelay(decoded, 0, retryAt.Add(-time.Minute)); got != time.Minute {
		t.Fatalf("retry-at changed: %v", got)
	}
	for _, old := range []error{nil, errors.New("untyped"), temporal.NewApplicationError("N-1", FailureTypeInfrastructure), temporal.NewApplicationError("future", FailureTypeInfrastructure, retryAt, dispatchFailureEvidence{SchemaVersion: 2}), temporal.NewApplicationError("unknown", FailureTypeInfrastructure, "other-detail")} {
		if len(failedArtifactReceipts(old)) != 0 {
			t.Fatalf("invented receipts for %v", old)
		}
	}
}

func TestArtifactReceiptsAreInvocationScoped(t *testing.T) {
	first, a := collectArtifactReceipts(t.Context())
	second, b := collectArtifactReceipts(t.Context())
	receipt := fixtureArtifactReceipt(t)
	invoke.ArtifactReporter(first)(receipt)
	invoke.ArtifactReporter(second)(invoke.ArtifactReceipt{Name: "other", Ref: receipt.Ref})
	var result stageActivityResult
	var err error
	finishArtifactReceipts(a, &result, &err)
	if len(result.RecordedArtifacts) != 1 || result.RecordedArtifacts[0] != receipt {
		t.Fatalf("first result %+v", result.RecordedArtifacts)
	}
	err = classifySeamError(errors.New("failed"))
	finishArtifactReceipts(b, &result, &err)
	if got := failedArtifactReceipts(err); len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("second failure %+v", got)
	}
}

func TestArtifactReceiptsVersionGateAndRecordOrder(t *testing.T) {
	for _, version := range []workflow.Version{workflow.DefaultVersion, 1} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.OnGetVersion(artifactReceiptsChange, workflow.DefaultVersion, 1).Return(version)
			receipt := fixtureArtifactReceipt(t)
			env.ExecuteWorkflow(func(ctx workflow.Context) (JournalProjection, error) {
				rec := &runJournal{}
				rec.append(ctx, journal.Event{Type: journal.EventStageStarted, Stage: "work", Attempt: 1})
				if err := rec.recordArtifactReceipts(ctx, []invoke.ArtifactReceipt{receipt, receipt}, nil); err != nil {
					return JournalProjection{}, err
				}
				rec.append(ctx, journal.Event{Type: journal.EventStageFinished, Stage: "work", Attempt: 1, Status: "success"})
				return rec.proj, nil
			})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var got JournalProjection
			if err := env.GetWorkflowResult(&got); err != nil {
				t.Fatal(err)
			}
			if version == workflow.DefaultVersion {
				if len(got.Ops) != 2 {
					t.Fatalf("N-1 added receipts %+v", got.Ops)
				}
			} else {
				if len(got.Ops) != 4 || got.Ops[1].Artifact == nil || got.Ops[2].Artifact == nil || got.Ops[3].Event.Type != journal.EventStageFinished {
					t.Fatalf("record order %+v", got.Ops)
				}
				for _, op := range got.Ops[1:3] {
					if op.Artifact.Name != receipt.Name || op.Artifact.Ref.Digest != receipt.Ref.Digest || op.Artifact.Stage != "" {
						t.Fatalf("recorder identity changed %+v", op.Artifact)
					}
				}
			}
		})
	}
}

type receiptExecutor struct {
	receipt invoke.ArtifactReceipt
	fail    bool
}

func (e receiptExecutor) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	invoke.ArtifactReporter(ctx)(e.receipt)
	if e.fail {
		return apiv1.ResultEnvelope{}, errors.New("recorded before failure")
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: []apiv1.ArtifactPointer{e.receipt.Ref}}, nil
}

func TestDeterministicActivityCarriesRecorderReceipts(t *testing.T) {
	for _, fail := range []bool{false, true} {
		receipt := fixtureArtifactReceipt(t)
		activities := Activities{Det: receiptExecutor{receipt: receipt, fail: fail}, Workspaces: testWorkspaces(t)}
		got, err := activities.RunDeterministic(t.Context(), apiv1.InvocationEnvelope{RunID: "receipt-run", TaskID: "receipt-run:work", RepoRef: apiv1.RepoRef{Name: "fixture"}}, apiv1.DeterministicRun{Command: []string{"fixture"}}, "", "")
		receipts := got.RecordedArtifacts
		if fail {
			if err == nil {
				t.Fatal("expected failure")
			}
			receipts = failedArtifactReceipts(err)
		} else if err != nil {
			t.Fatal(err)
		}
		if len(receipts) != 1 || receipts[0] != receipt {
			t.Fatalf("activity receipts = %+v", receipts)
		}
	}
}
