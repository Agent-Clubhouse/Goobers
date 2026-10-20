package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestEngineChildContinuationIntegrity(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote=%v", remote), func(t *testing.T) {
			testEngineChildContinuationIntegrity(t, remote)
		})
	}
}

func testEngineChildContinuationIntegrity(t *testing.T, remote bool) {
	t.Run("admission", func(t *testing.T) {
		f := newECWFixture()
		env, _ := ecwEnvironment(t, f)
		in := ecwInput()
		configureChildIntegrityRoute(env, f, &in, remote)
		in.Spec.Tasks[0].MinimumIntegrity = apiv1.IntegrityMaintainer
		env.ExecuteWorkflow("Run", in)
		if len(f.envelopes) != 1 {
			t.Fatalf("resumed %d parent invocations despite minimumIntegrity=maintainer and unapproved child context: %+v", len(f.envelopes), f.envelopes[len(f.envelopes)-1].ContextPointers)
		}
		if env.GetWorkflowError() == nil {
			t.Fatal("unapproved continuation did not fail admission")
		}
	})
	t.Run("provenance", func(t *testing.T) {
		f := newECWFixture()
		f.waits = 2
		f.parentIntegrity = apiv1.IntegrityTrusted
		env, dir := ecwEnvironment(t, f)
		in := ecwInput()
		configureChildIntegrityRoute(env, f, &in, remote)
		env.ExecuteWorkflow("Run", in)
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		reader, err := journal.OpenRead(filepath.Join(dir, in.RunID))
		if err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == journal.EventStageFinished && event.Stage == "parent" {
				if event.Integrity != apiv1.IntegrityUnapproved {
					t.Fatalf("parent output grade=%q despite consuming unapproved child continuation", event.Integrity)
				}
				return
			}
		}
		t.Fatal("missing parent stage completion")
	})
}

func configureChildIntegrityRoute(env *testsuite.TestWorkflowEnvironment, f *ecwFixture, in *RunInput, remote bool) {
	if !remote {
		return
	}
	in.Placements = []PinnedPlacement{{Stage: "parent", Queue: "child-integrity", Eligible: remoteEligible()}}
	env.RegisterActivityWithOptions(func(ctx context.Context, input DispatchStageInput) (DispatchStageResult, error) {
		return f.Invoke(ctx, input.Envelope, input.WorkspaceBranch, input.WorkspaceDelta, input.Workspace, "")
	}, activity.RegisterOptions{Name: ActDispatchStage})
}
