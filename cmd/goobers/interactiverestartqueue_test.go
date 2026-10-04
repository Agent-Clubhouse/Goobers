package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/restartintent"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (f *humanAcceptanceFixture) restartCommand(t *testing.T) apicontract.InteractiveRunCommand {
	t.Helper()
	reader, err := journal.OpenReadOnly(filepath.Join(f.pinned.layout.ForGaggle("example").RunsDir(), f.pinned.parent.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var seq uint64
	for _, e := range events {
		if e.Type == journal.EventRunFinished {
			seq = e.Seq
		}
	}
	saved := f.command(t, "queue-guidance", apicontract.InteractiveRunCommand{Kind: "guidance", Stage: "plan", ExpectedSubjectSequence: seq, Guidance: "Preserve this exact instruction"})
	return apicontract.InteractiveRunCommand{Kind: "restart", Stage: "plan", ExpectedSubjectSequence: seq, GuidanceIDs: []string{saved.Guidance.Request.RequestID}, Rationale: "Reviewed queued epoch"}
}

func TestHumanRestartQueueAcceptsWhileCapacityUnavailable(t *testing.T) {
	f := newHumanAcceptanceFixture(t)
	release, ok, reason := f.scheduler.ReserveContinuation("other-owner", "example", f.pinned.parent.Workflow)
	if !ok {
		t.Fatal(reason)
	}
	defer release()
	command := f.restartCommand(t)
	accepted := f.command(t, "capacity-queued", command)
	if accepted.Status != "pending" {
		t.Fatal(accepted)
	}
	record, err := f.durable.queue.ByKey(t.Context(), restartintent.Key(accepted.ContinuationRunID))
	if err != nil || record.State != triggerqueue.Accepted {
		t.Fatal(record, err)
	}
	pins := map[string]bool{}
	if err := retainEventGenerationPins(t.Context(), f.pinned.layout, pins); err != nil || !pins[f.pinned.parent.ConfigGeneration] {
		t.Fatal("queued generation not retained", pins, err)
	}
	sourceDir := filepath.Join(f.pinned.layout.ForGaggle("example").RunsDir(), f.pinned.parent.RunID)
	if err := protectEventJournal(t.Context(), f.durable.queue, retention.Result{RunID: f.pinned.parent.RunID, RunDir: sourceDir}); !errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatal("queued restart lost source retention", err)
	}
	dir := filepath.Join(f.pinned.layout.ForGaggle("example").RunsDir(), accepted.ContinuationRunID)
	if _, err = os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("epoch created before capacity", err)
	}
	if err = f.durable.Drain(t.Context()); err == nil {
		t.Fatal("capacity unexpectedly admitted")
	}
	record, err = f.durable.queue.ByKey(t.Context(), record.Key)
	if err != nil || record.State != triggerqueue.Accepted {
		t.Fatal("refusal lost custody", record, err)
	}
	if record.Reason != triggerqueue.WaitingCapacity.Message() {
		t.Fatal("missing capacity explanation", record)
	}
	waiting := f.command(t, "capacity-queued", command)
	if waiting.PendingReason != record.Reason || waiting.Status != "pending" {
		t.Fatal("receipt lost waiting explanation", waiting)
	}
	f.process.mu.Lock()
	count := len(f.process.requests)
	f.process.mu.Unlock()
	if count != 0 {
		t.Fatal("model executed before admission")
	}
	release()
	if err = f.durable.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wg.Wait()
	replay := f.command(t, "capacity-queued", command)
	if replay.Status != "started" || replay.ContinuationRunID != accepted.ContinuationRunID {
		t.Fatal(replay)
	}
}

func TestHumanRestartQueuedReplayIgnoresSourceMutationAndRevocationStopsDrain(t *testing.T) {
	f := newHumanAcceptanceFixture(t)
	command := f.restartCommand(t)
	accepted := f.command(t, "retained-command", command)
	path := filepath.Join(f.pinned.layout.ForGaggle("example").RunsDir(), f.pinned.parent.RunID)
	run, _, err := journal.Recover(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	replay := f.command(t, "retained-command", command)
	if replay.ContinuationRunID != accepted.ContinuationRunID || replay.Status != "pending" {
		t.Fatal("source mutation replaced accepted command", replay)
	}
	record, err := f.durable.queue.ByKey(t.Context(), restartintent.Key(accepted.ContinuationRunID))
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.durable.queue.HumanRestartPlan(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed := f.setup.Definitions.Gaggles[0].DeepCopy()
	changed.Spec.InteractiveAccess.Humans.Operators = nil
	if err = f.setup.InteractiveAccess.Apply([]apiv1.Gaggle{*changed}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err = f.durable.Drain(t.Context()); !errors.Is(err, interactiveaccess.ErrDenied) {
		t.Fatal("revoked drain did not refuse", err)
	}
	retained, err := f.durable.queue.HumanRestartPlan(t.Context(), record.ID)
	if err != nil || !bytes.Equal(original, retained) {
		t.Fatal("accepted context changed", err)
	}
	latest, _ := f.durable.queue.ByKey(t.Context(), record.Key)
	if latest.State != triggerqueue.Accepted {
		t.Fatal("revocation lost custody", latest)
	}
	f.process.mu.Lock()
	count := len(f.process.requests)
	f.process.mu.Unlock()
	if count != 0 {
		t.Fatal("revoked human executed")
	}
}

func TestHumanRestartPublicationBarrierFailureHasNoExecution(t *testing.T) {
	f := newHumanAcceptanceFixture(t)
	command := f.restartCommand(t)
	accepted := f.command(t, "barrier", command)
	record, err := f.durable.queue.ByKey(t.Context(), restartintent.Key(accepted.ContinuationRunID))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.durable.restarts.Load(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	blocked := errors.New("durable barrier refused")
	err = f.durable.restarts.Launch(t.Context(), context.Background(), plan, func(context.Context) error { return blocked })
	if !errors.Is(err, blocked) {
		t.Fatal(err)
	}
	f.wg.Wait()
	dir := filepath.Join(f.pinned.layout.ForGaggle("example").RunsDir(), accepted.ContinuationRunID)
	if _, err = os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published before durable barrier", err)
	}
	if err = f.durable.Drain(t.Context()); err != nil {
		t.Fatal("capacity/claims leaked after failed barrier", err)
	}
	f.wg.Wait()
}
