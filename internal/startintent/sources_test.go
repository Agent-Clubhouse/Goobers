package startintent

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

func sourceFixture(t *testing.T) (*Sources, localscheduler.WorkflowEntry) {
	t.Helper()
	service := intentService(t)
	return &Sources{Queue: service.Queue, Acquire: func(context.Context, Target) (func(), error) { return func() {}, nil }}, localscheduler.WorkflowEntry{Gaggle: "own", Workflow: "worker", ConfigGeneration: "generation", WorkflowDigest: "workflow", GooberDigest: "goober"}
}
func TestSourceSignalPinsRecipientSetAndNoMatch(t *testing.T) {
	source, entry := sourceFixture(t)
	now := time.Now().UTC()
	delivery := &webhookhttp.Delivery{Event: "issues", ID: "delivery", PayloadDigest: "first"}
	ids, err := source.AcceptSignal(t.Context(), []localscheduler.WorkflowEntry{entry}, delivery.ID, "github-webhook:issues", "github-webhook:issues", delivery, now)
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	source.Acquire = func(context.Context, Target) (func(), error) { t.Fatal("replay recaptured config"); return nil, nil }
	changed := entry
	changed.ConfigGeneration = "changed"
	changed.Gaggle = "other"
	replay, err := source.AcceptSignal(t.Context(), []localscheduler.WorkflowEntry{changed}, delivery.ID, "github-webhook:issues", "github-webhook:issues", delivery, now)
	if err != nil || !reflect.DeepEqual(replay, ids) {
		t.Fatal(replay, err)
	}
	delivery.PayloadDigest = "changed"
	if _, err = source.AcceptSignal(t.Context(), nil, delivery.ID, "github-webhook:issues", "github-webhook:issues", delivery, now); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal(err)
	}
	empty, err := source.AcceptSignal(t.Context(), nil, "no-match", "deploy", "deploy", nil, now)
	if err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
	empty, err = source.AcceptSignal(t.Context(), []localscheduler.WorkflowEntry{changed}, "no-match", "deploy", "deploy", nil, now)
	if err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
}
