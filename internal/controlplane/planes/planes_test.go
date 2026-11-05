package planes

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/localscheduler"
)

type triggerStub struct {
	calls int
}

func (s *triggerStub) TriggerWithDispatchContextOptions(context.Context, context.Context, string, time.Time, localscheduler.ManualTriggerOptions) (string, error) {
	s.calls++
	return "run-1", nil
}
func (s *triggerStub) TriggerExactWithDispatchContextOptions(context.Context, context.Context, localscheduler.WorkflowIdentity, time.Time, localscheduler.ManualTriggerOptions) (string, error) {
	s.calls++
	return "run-1", nil
}
func (s *triggerStub) TriggerPriorityWithDispatchContext(context.Context, context.Context, localscheduler.WorkflowIdentity, string, time.Time) (string, error) {
	s.calls++
	return "run-1", nil
}

func TestTriggerServiceDeduplicatesRequestIdentity(t *testing.T) {
	dispatch := &triggerStub{}
	service := NewTriggerService()
	service.AttachDispatcher(dispatch)
	request := httpapi.TriggerRequest{Workflow: "implementation", RequestID: "delivery-1"}
	first, err := service.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.RunID != "run-1" || second.RunID != first.RunID || !second.Duplicate || dispatch.calls != 1 {
		t.Fatalf("first=%+v second=%+v calls=%d", first, second, dispatch.calls)
	}
}

func TestRunJournalServiceContainsBothRunIdentities(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	for _, runID := range []string{"asking", "target"} {
		run, err := journal.Create(layout.ForGaggle("web").RunsDir(), journal.RunIdentity{
			RunID: runID, Gaggle: "web", Workflow: "implementation", WorkflowVersion: 1,
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	service := NewRunJournalService(layout, nil)
	response, err := service.RunPhase(t.Context(), journalclient.RunPhaseRequest{
		RunID: "asking", Gaggle: "web", TargetRunID: "target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RunID != "target" || response.Phase != string(journal.PhaseRunning) {
		t.Fatalf("response = %+v", response)
	}
	_, err = service.RunPhase(t.Context(), journalclient.RunPhaseRequest{
		RunID: "asking", Gaggle: "web", TargetRunID: "foreign",
	})
	if err == nil {
		t.Fatal("foreign target was not refused")
	}
}
