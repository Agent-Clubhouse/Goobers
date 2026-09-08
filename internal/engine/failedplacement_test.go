package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestDispatchFailurePlacementSurvivesTemporalEncodingWithoutChangingRetry(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	retryAt := now.Add(17 * time.Minute)
	report := dispatcher.Report{Runner: "actual-runner", Pod: "observed-pod", Image: "actual-image", Node: "observed-node", OS: "linux", QueuedAt: now, PodStartedAt: now.Add(time.Second)}
	for _, retryReset := range []bool{false, true} {
		var cause error = invoke.InfrastructureFailure(errors.New("pod lost"))
		if retryReset {
			cause = invoke.InfrastructureFailureUntil(errors.New("pod lost"), retryAt)
		}
		classified := classifySeamError(cause)
		originalDelay := infrastructureRetryDelay(classified, time.Second, now)
		encoded := withDispatchFailurePlacement(classified, report)
		converter := temporal.GetDefaultFailureConverter()
		decoded := converter.FailureToError(converter.ErrorToFailure(encoded))
		if got := DispatchFailurePlacement(fmt.Errorf("client context: %w", decoded)); !reflect.DeepEqual(got, placementProvenance(report)) {
			t.Fatalf("placement after SDK round trip = %+v", got)
		}
		if class, err := ClassifyDispatchFailure(decoded); err != nil || class != journal.AttemptInfra {
			t.Fatalf("failure class changed: %q, %v", class, err)
		}
		if got := infrastructureRetryDelay(decoded, time.Second, now); got != originalDelay {
			t.Fatalf("retry reset changed: %v, want %v", got, originalDelay)
		}
	}
}

func TestDispatchFailurePlacementToleratesOlderUnknownAndPartialEvidence(t *testing.T) {
	queued := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	partial := dispatcher.Report{Runner: "selected-runner", QueuedAt: queued}
	err := withDispatchFailurePlacement(classifyDispatchError(&dispatcher.SkewError{Image: "stage:other", Reason: "different build"}), partial)
	got := DispatchFailurePlacement(err)
	if got == nil || got.Runner != partial.Runner || got.Pod != "" || got.Node != "" || got.OS != "" || got.Image != "" || !got.PodStartedAt.IsZero() {
		t.Fatalf("pre-create refusal invented observations: %+v", got)
	}
	if class, classErr := ClassifyDispatchFailure(err); classErr != nil || class != journal.AttemptPolicy {
		t.Fatalf("deterministic refusal changed class: %q, %v", class, classErr)
	}
	for _, old := range []error{
		nil,
		errors.New("untyped"),
		temporal.NewApplicationError("old", FailureTypeInfrastructure),
		temporal.NewApplicationError("old reset", FailureTypeInfrastructure, queued),
		temporal.NewApplicationError("future", FailureTypeInfrastructure, queued, dispatchFailureEvidence{SchemaVersion: 2, Placement: got}),
		temporal.NewApplicationError("malformed", FailureTypeInfrastructure, queued, "not evidence"),
		temporal.NewApplicationError("other domain", "other", queued, dispatchFailureEvidence{SchemaVersion: 1, Placement: got}),
	} {
		if p := DispatchFailurePlacement(old); p != nil {
			t.Fatalf("unknown error invented placement: %+v", p)
		}
	}
	unknown := temporal.NewApplicationError("existing details", FailureTypeInfrastructure, map[string]string{"future": "data"})
	if withDispatchFailurePlacement(unknown, partial) != unknown {
		t.Fatal("unrecognized existing details were overwritten")
	}
	extension := temporal.NewApplicationError("existing extension", FailureTypeInfrastructure, queued, "other data")
	if withDispatchFailurePlacement(extension, partial) != extension {
		t.Fatal("another detail extension was overwritten")
	}
}

type placementDispatchFunc func(context.Context, dispatcher.Attempt, []dispatcher.RunnerSpec) (dispatcher.Report, error)

func (f placementDispatchFunc) Dispatch(ctx context.Context, a dispatcher.Attempt, r []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	return f(ctx, a, r)
}

func TestFailedPodPlacementReachesWorkflowJournalBeforeSuccessfulRetry(t *testing.T) {
	in := runInput("failed-pod-placement", apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "build",
		Tasks: []apiv1.Task{podTask("build", "", nil)},
	})
	in.Placements = []PinnedPlacement{remotePin("build")}
	store := surrenderStore(t)
	putSurrendered(t, store, in.RunID, "build", 2, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	observed := placementDispatchFunc(func(_ context.Context, attempt dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
		report := dispatcher.Report{Runner: "actual-runner", Pod: fmt.Sprintf("observed-pod-%d", attempt.Number), Image: "actual-image", Node: "actual-node", OS: "linux", QueuedAt: time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)}
		if attempt.Number == 1 {
			return report, errors.New("observed pod was deleted")
		}
		report.SurrenderConfirmed = true
		report.Phase = corev1.PodSucceeded
		return report, nil
	})
	projection := executeForProjection(t, in, &Activities{Workspaces: testWorkspaces(t), Dispatcher: observed, Surrenders: store}, false)
	placements := projectedPlacements(t, projection)
	for attempt := 1; attempt <= 2; attempt++ {
		got, ok := placements[fmt.Sprintf("build#%d", attempt)]
		if !ok || got.Pod != fmt.Sprintf("observed-pod-%d", attempt) || got.Node != "actual-node" || got.OS != "linux" || got.Runner != "actual-runner" {
			t.Fatalf("attempt %d placement = %+v, present=%t", attempt, got, ok)
		}
	}
}
