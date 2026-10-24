package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type noAcceptances struct {
	triggerqueue.SchedulerPolicyStore
}

func (noAcceptances) Accept(context.Context, string, string, []byte, time.Time) (triggerqueue.Record, bool, error) {
	return triggerqueue.Record{}, false, errors.New("must not be reached")
}

func (noAcceptances) ByKey(context.Context, string) (triggerqueue.Record, error) {
	return triggerqueue.Record{}, sql.ErrNoRows
}

func TestConstructTriggersComposesAndFailsClosedUntilConfigured(t *testing.T) {
	var planes controlPlanes
	if err := constructTriggers(context.Background(), databaseServices{}, controlAuth{}, nil, nil, &planes); err == nil {
		t.Fatal("trigger plane composed without an acceptance store")
	}
	if err := constructTriggers(context.Background(), databaseServices{Acceptances: noAcceptances{}}, controlAuth{}, nil, nil, &planes); err != nil {
		t.Fatal(err)
	}
	if planes.Triggers == nil {
		t.Fatal("constructor did not compose the Triggers seam")
	}
	// No Configuration and no pin yet: the plane refuses without touching the store.
	_, err := planes.Triggers.Trigger(context.Background(), httpapi.TriggerRequest{Gaggle: "g", Workflow: "w", RequestID: "k"})
	var intervention *httpapi.InterventionError
	if !errors.As(err, &intervention) || intervention.Status != http.StatusServiceUnavailable {
		t.Fatalf("trigger = %v, want 503 until configuration and pinning are composed", err)
	}
}
