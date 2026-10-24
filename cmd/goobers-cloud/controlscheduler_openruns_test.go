package main

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func dispatchingRecord(t *testing.T, runID string) triggerqueue.Record {
	t.Helper()
	payload, err := startintent.Envelope{
		Kind:    startintent.Kind,
		Request: startintent.Request{Workflow: testWorkflow},
		Target:  startintent.Target{Gaggle: testGaggle, Workflow: testWorkflow, ConfigGeneration: "g", WorkflowDigest: "w", GooberDigest: "d"},
	}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return triggerqueue.Record{ID: "trigger-" + runID, State: triggerqueue.Dispatching, Payload: payload}
}

func TestCollectOpenRunsKeepsOnlyRunsThatMayHoldASlot(t *testing.T) {
	pins := []acceptance.RunPins{
		{RunID: "closed", Gaggle: testGaggle, Workflow: testWorkflow},
		{RunID: "open", Gaggle: testGaggle, Workflow: testWorkflow},
		{RunID: "unconfirmable", Gaggle: testGaggle, Workflow: testWorkflow},
	}
	out, err := collectOpenRuns(t.Context(), openRunInputs{
		Pins: pins,
		Closed: func(_ context.Context, runID string) (bool, error) {
			switch runID {
			case "closed":
				return true, nil
			case "unconfirmable":
				return false, errors.New("temporal unavailable")
			}
			return false, nil
		},
		// "open" is both pinned and still 'dispatching': one slot, not two.
		Uncertain: []triggerqueue.Record{dispatchingRecord(t, "open"), dispatchingRecord(t, "uncertain-only")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, run := range out {
		ids = append(ids, run.RunID)
		if run.Identity != (localscheduler.WorkflowIdentity{Gaggle: testGaggle, Workflow: testWorkflow}) {
			t.Fatalf("run %s identity = %+v", run.RunID, run.Identity)
		}
	}
	want := []string{"open", "uncertain-only", "unconfirmable"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("open runs = %v, want %v (closed excluded; failed confirmation and dispatching rows held)", ids, want)
	}
}

func TestCollectOpenRunsStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := collectOpenRuns(ctx, openRunInputs{Pins: []acceptance.RunPins{{RunID: "r"}}, Closed: func(ctx context.Context, _ string) (bool, error) { return false, ctx.Err() }}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
