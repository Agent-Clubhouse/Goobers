package engine

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

func TestDeclaredOutboxRequiresCapableWriter(t *testing.T) {
	store := surrenderStore(t)
	putSurrendered(t, store, "legacy-outbox", "build", 1, dispatcher.SurrenderedResult{
		ContractVersion: dispatcher.SurrenderContractV2,
		Result:          apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	activities := &Activities{
		Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", SurrenderConfirmed: true}},
		Surrenders: store,
	}
	input := dispatchInput("legacy-outbox", "build", 1)
	input.Outbox = []string{"report.txt"}
	if result, err := activities.DispatchStage(t.Context(), input); err == nil {
		t.Fatalf("declared outbox silently accepted from v2 writer: status=%s outbox=%v", result.Status, result.Outbox)
	}
}

func TestDeclaredOutboxAcceptsAcknowledgedEmptyExport(t *testing.T) {
	store := surrenderStore(t)
	putSurrendered(t, store, "empty-outbox", "build", 1, dispatcher.SurrenderedResult{
		ContractVersion: dispatcher.SurrenderContractV3,
		OutboxExported:  true,
		Result:          apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	activities := &Activities{
		Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", SurrenderConfirmed: true}},
		Surrenders: store,
	}
	input := dispatchInput("empty-outbox", "build", 1)
	input.Outbox = []string{"optional-report.txt"}
	if _, err := activities.DispatchStage(t.Context(), input); err != nil {
		t.Fatalf("acknowledged empty export refused: %v", err)
	}
}
