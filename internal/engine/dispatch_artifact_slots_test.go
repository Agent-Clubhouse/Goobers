package engine

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

func TestDispatchStageCarriesDeterministicNamedPublication(t *testing.T) {
	store := surrenderStore(t)
	putSurrendered(t, store, "named", "produce", 2, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true, Disposed: true}}
	input := dispatchInput("named", "produce", 2)
	input.Run = &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}
	input.Envelope.ArtifactPublication = &apiv1.ArtifactPublication{Stage: "produce", Visit: 21, Slots: []apiv1.ArtifactSlot{{Name: "report"}}}
	activities := &Activities{Dispatcher: fake, Surrenders: store}
	if _, err := activities.DispatchStage(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	attempts, _ := fake.recorded()
	if len(attempts) != 1 || !reflect.DeepEqual(attempts[0].ArtifactPublication, input.Envelope.ArtifactPublication) || attempts[0].Number != 2 {
		t.Fatalf("named publication lost at remote deterministic boundary: %+v", attempts)
	}
}
