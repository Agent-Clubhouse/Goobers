package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func dependencyEvent(generation, source string) triggerqueue.EventAcceptance {
	return triggerqueue.EventAcceptance{
		Producer: triggerqueue.EventProducer{Gaggle: "example", Binding: "workflow", Actor: "run:" + source, RunID: source, Stage: "emit", RootID: source},
		Envelope: []byte(`{"specversion":"1.0","source":"/factory","id":"1","type":"test.ready"}`),
		Plan:     eventing.Plan{Revision: "routing", Routes: []eventing.Route{{Consumer: "consumer", Revision: "subscription", Workflow: "default-implement", WorkflowDigest: "workflow", GooberDigest: "goober", ConfigGeneration: generation}}},
	}
}

func TestEventDependenciesProtectJournalBeforeRoutingAndAfterRootExpiry(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	if err := os.MkdirAll(layout.SchedulerDir(), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	source := strings.Repeat("a", 32)
	dir := createTelemetryRetentionRun(t, layout.ForGaggle("example"), source, now.Add(-48*time.Hour))
	s := acceptedService(t, filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService())
	if _, _, err := s.queue.AcceptEvent(t.Context(), dependencyEvent("generation", source), now); err != nil {
		t.Fatal(err)
	}
	guard, closeGuard, err := openTriggerPruneGuard(layout, false, now)
	if err != nil {
		t.Fatal(err)
	}
	defer closeGuard()
	candidate := retention.Result{RunID: source, RunDir: dir}
	if err = guard(candidate); !errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatal("unrouted event lost source journal", err)
	}
	routed, err := s.queue.RouteNextEvent(t.Context(), "example", now)
	if err != nil || len(routed.Groups) != 1 {
		t.Fatal(routed, err)
	}
	record, start, err := s.queue.VerifiedEventStart(t.Context(), "example", routed.Groups[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = s.queue.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.queue.Finish(t.Context(), record.ID, triggerqueue.Rejected, "", "fixture refusal", now); err != nil {
		t.Fatal(err)
	}
	if err = s.queue.SettleEventGroup(t.Context(), "example", start.GroupID, "", "rejected", now); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now.Add(triggerqueue.EventRetention), now.Add(triggerqueue.EventRetention + triggerqueue.EventTombstoneRetention)} {
		if _, err = s.queue.PruneEvents(t.Context(), at, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err = guard(candidate); !errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatal("root budget lost source journal after receipt expiry", err)
	}
	foreignRun, err := journal.Create(layout.ForGaggle("foreign").RunsDir(), journal.RunIdentity{RunID: source, Gaggle: "foreign", Workflow: "other", WorkflowVersion: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = foreignRun.Close(); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(layout.ForGaggle("foreign").RunsDir(), source)
	if err = guard(retention.Result{RunID: source, RunDir: foreign}); err != nil {
		t.Fatal("dependency crossed gaggle boundary", err)
	}
}

func TestEventPendingReceiptProtectsArchiveAtActualCapacityPrune(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	if err := os.MkdirAll(layout.SchedulerDir(), 0700); err != nil {
		t.Fatal(err)
	}
	owner, err := layout.EnsureIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	store.DurablePins = func(ctx context.Context) (map[string]bool, error) {
		return retainedExecutionGenerationPins(ctx, layout)
	}
	queue := acceptedService(t, filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService()).queue
	now := time.Now().UTC()
	var first string
	for i := range configgeneration.MaxGenerations {
		writeFixture(t, filepath.Join(layout.ConfigDir(), "generation-note.txt"), fmt.Sprint(i))
		data, digest, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), owner)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			_, lease, err := store.KeepAndAcquire(t.Context(), data, digest, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, acceptErr := queue.AcceptEvent(t.Context(), dependencyEvent(digest, "source"), now)
			err = lease.Release()
			if acceptErr != nil || err != nil {
				t.Fatal(acceptErr, err)
			}
			first = digest
		} else {
			_, lease, err := store.KeepExternallyOwned(t.Context(), data, digest)
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeFixture(t, filepath.Join(layout.ConfigDir(), "generation-note.txt"), "overflow")
	data, digest, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Keep(t.Context(), data, digest, nil); err == nil {
		t.Fatal("archive pruning ignored accepted, unrouted custody")
	}
	if _, err = store.Load(t.Context(), first); err != nil {
		t.Fatal("accepted generation lost", err)
	}
	for _, at := range []time.Time{now.Add(triggerqueue.EventRoutingDeadline), now.Add(triggerqueue.EventRoutingDeadline + triggerqueue.EventRetention)} {
		if _, err = queue.PruneEvents(t.Context(), at, 100); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.Keep(t.Context(), data, digest, nil); err != nil {
		t.Fatal("expired unrouted custody did not release archive", err)
	}
}
