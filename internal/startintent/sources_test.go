package startintent

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
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
func TestSourceScheduleRecoversQueueCursorAndUsesPinnedStarter(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Minute)
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), base.Add(time.Minute))
	pending, err := source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	accepted, err := Parse(pending[0].Payload)
	if err != nil || accepted.Source == nil || !accepted.Source.ScheduledFrom.Equal(base) {
		t.Fatal(accepted, err)
	}
	evaluations, err := localscheduler.ReadTriggerEvaluations(log.Dir())
	if err != nil || !evaluations[localscheduler.WorkflowIdentity{Gaggle: entry.Gaggle, Workflow: entry.Workflow}].Equal(base.Add(time.Minute)) {
		t.Fatal(evaluations, err)
	}

	// Simulate a stale/ahead legacy evaluation file; durable cursor controls restart.
	restarted := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = restarted.ReconcileAll(nil, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	restarted.Tick(t.Context(), base.Add(time.Minute))
	pending, err = source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	restarted.Tick(t.Context(), base.Add(5*time.Minute))
	pending, err = source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	// Any number of missed ticks still produces one catch-up worker, as before.
	last, err := Parse(pending[1].Payload)
	if err != nil || !last.Source.ScheduledFrom.Equal(base.Add(time.Minute)) || !last.Source.ScheduledAt.Equal(base.Add(5*time.Minute)) {
		t.Fatal(last, err)
	}
}
func TestSourceScheduleArchiveFailureLeavesDueCursor(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC()
	if _, _, err := source.ScheduleCursor(t.Context(), entry, base, false); err != nil {
		t.Fatal(err)
	}
	source.Acquire = func(context.Context, Target) (func(), error) { return nil, errors.New("archive unavailable") }
	if err := source.AcceptSchedule(t.Context(), entry, base, base.Add(time.Minute), true); err == nil {
		t.Fatal("missing pins accepted")
	}
	if cursor, _, err := source.ScheduleCursor(t.Context(), entry, base.Add(time.Hour), false); err != nil || !cursor.Equal(base) {
		t.Fatal(cursor, err)
	}
}
