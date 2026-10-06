package engine

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildOriginAgenticDispatchCopy(t *testing.T) {
	store := surrenderStore(t)
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	putSurrendered(t, store, "parent", "work", 2, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	input := dispatchInput("parent", "work", 2)
	input.Envelope.Goober = "coder"
	input.Envelope.ChildWorkflowOrigin = &apiv1.ChildWorkflowOrigin{StageOccurrence: journal.StageAttemptID("parent", 1, "work", 2), AttemptID: journal.StageAttemptID("parent", 1, "work", 7)}
	activities := &Activities{Dispatcher: fake, Surrenders: store}
	if _, err := activities.DispatchStage(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	attempts, _ := fake.recorded()
	if len(attempts) != 1 || attempts[0].Envelope == nil || !reflect.DeepEqual(attempts[0].Envelope.ChildWorkflowOrigin, input.Envelope.ChildWorkflowOrigin) {
		t.Fatalf("dispatch lost origin: %+v", attempts)
	}
	input.Envelope.ChildWorkflowOrigin.AttemptID = "changed-after-dispatch"
	if attempts[0].Envelope.ChildWorkflowOrigin.AttemptID == input.Envelope.ChildWorkflowOrigin.AttemptID {
		t.Fatal("dispatched origin aliases caller-owned metadata")
	}
}

func TestChildOriginTemporalDoesNotInventProjectionAuthority(t *testing.T) {
	task := apiv1.Task{Name: "parent", Type: apiv1.TaskAgentic, Goober: "coder", ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}
	record := &runJournal{}
	record.stageStarted(time.Now(), task, 1, "")
	env := record.taskAttemptEnvelope(apiv1.InvocationEnvelope{}, task, 1)
	if env.ChildWorkflowOrigin != nil || record.proj.Ops[0].Event.Runner[journal.ChildWorkflowOccurrenceKey] != nil {
		t.Fatal("Temporal projection ordinal was presented as committed child authority")
	}
}
