package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func requestInitialChildControl(t *testing.T, s *durableTriggerService, ref childExecutionRef) triggerqueue.StartControl {
	t.Helper()
	record, err := s.queue.ChildStart(t.Context(), ref.Child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	scope := triggerqueue.StartScope{Gaggle: ref.Envelope.Gaggle, Workflow: ref.Envelope.Workflow, Kind: childworkflow.ChildStartKind, Source: "child", Generation: ref.Envelope.ConfigGeneration, PayloadDigest: journal.Digest(record.Payload), ReservedRunID: ref.Child.RunID}
	if _, err = s.queue.PinStartControl(t.Context(), record.ID, scope); err != nil {
		t.Fatal(err)
	}
	control, _, err := s.queue.RequestStartCancellation(t.Context(), scope.Gaggle, record.ID, triggerqueue.StartCancellation{RequestID: "stop-original", Actor: "verified-human", Reason: "No longer needed", Authority: []byte(`{"issuer":"trusted","subject":"operator"}`)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return control
}
func currentChildRef(t *testing.T, f *actualChildFixture) childExecutionRef {
	t.Helper()
	_, record := f.state(t)
	ref, err := f.service.childReference(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestControlledChildCancellationStopsExactLiveRunnerAndPreservesParentResultCustody(t *testing.T) {
	f := actualChildLaunchFixture(t)
	f.launcher.reconcile = func(context.Context, *journal.Reader) error { return nil }
	f.launcher.result = f.launcher.captureTerminal
	f.executor.entered, f.executor.proceed = make(chan struct{}), make(chan struct{})
	defer close(f.executor.proceed)
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.executor.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("child did not start")
	}
	ref := currentChildRef(t, f)
	c := requestInitialChildControl(t, f.service, ref)
	observed, handled, err := f.service.cancelTypedQueueStart(t.Context(), c)
	if err != nil || !handled {
		t.Fatal(observed, handled, err)
	}
	f.wg.Wait()
	observed, _, err = f.service.cancelTypedQueueStart(t.Context(), c)
	if err != nil || observed.State != startcontrol.CancellationConfirmed {
		t.Fatal(observed, err)
	}
	for range 2 {
		if err = f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	child, receipt := f.state(t)
	if child.State != triggerqueue.ChildCancelled || child.ResultRef == "" || !child.AcknowledgedAt.IsZero() || child.CancellationRequested || receipt.State != triggerqueue.Dispatched {
		t.Fatal("cancellation altered family or lost result", child, receipt)
	}
	if f.executor.calls.Load() != 1 {
		t.Fatal("execution replayed")
	}
}

func TestControlledChildObservationPreservesUnknownCustodyAndAvoidsRecovery(t *testing.T) {
	f := actualChildLaunchFixture(t)
	identity := publishInterruptedChild(t, f)
	f.launcher.result = f.launcher.captureTerminal
	ref := currentChildRef(t, f)
	c := requestInitialChildControl(t, f.service, ref)
	f.launcher.reconcile = func(context.Context, *journal.Reader) error { return invoke.ErrWorkspaceNotQuiescent }
	observed, _, err := f.service.cancelTypedQueueStart(t.Context(), c)
	if !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) || observed.State != startcontrol.CancellationRequested {
		t.Fatal(observed, err)
	}
	f.launcher.reconcile = func(context.Context, *journal.Reader) error { return nil }
	release, ok := f.launcher.runners.acquireChildCustody(ref.Child.RunID)
	if !ok {
		t.Fatal("custody busy")
	}
	observed, _, err = f.service.cancelTypedQueueStart(t.Context(), c)
	release()
	if err != nil || observed.State != startcontrol.CancellationRequested {
		t.Fatal(observed, err)
	}
	observed, _, err = f.service.cancelTypedQueueStart(t.Context(), c)
	if err != nil || observed.State != startcontrol.CancellationRequested {
		t.Fatal("nonterminal invented completion", observed, err)
	}
	if _, err = f.launcher.resolveGeneration(t.Context(), identity); !errors.Is(err, childworkflow.ErrAuthorityChanged) {
		t.Fatal("cancelled original startup resolved", err)
	}
	if _, err = f.launcher.credentialCeiling(t.Context(), identity); !errors.Is(err, childworkflow.ErrAuthorityChanged) {
		t.Fatal("cancelled original minted scope", err)
	}
	f.authority.revoked.Store(true) // cancellation observation must not reacquire execution policy
	if _, err = f.launcher.Result(t.Context(), ref); err != nil {
		t.Fatal("cancelled recovery tried authority", err)
	}
	if f.builds.Load() != 1 || f.executor.calls.Load() != 0 {
		t.Fatal("cancelled interrupted child resumed")
	}
	bad := c
	bad.Scope.ReservedRunID = "another-run"
	if _, _, err = f.service.cancelTypedQueueStart(t.Context(), bad); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal("retarget accepted", err)
	}
}

func TestControlledChildNaturalCompletionIsAlreadyTerminal(t *testing.T) {
	f := actualChildLaunchFixture(t)
	f.launcher.reconcile = func(context.Context, *journal.Reader) error { return nil }
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wg.Wait()
	c := requestInitialChildControl(t, f.service, currentChildRef(t, f))
	observed, _, err := f.service.cancelTypedQueueStart(t.Context(), c)
	if err != nil || observed.State != startcontrol.CancellationAlreadyTerminal {
		t.Fatal(observed, err)
	}
}

func TestControlledOriginalChildDoesNotRetargetHumanEpoch(t *testing.T) {
	for _, phase := range []journal.RunPhase{journal.PhaseFailed, journal.PhaseEscalated} {
		t.Run(string(phase), func(t *testing.T) {
			f := actualChildLaunchFixture(t)
			id := publishInterruptedChild(t, f)
			f.launcher.reconcile = func(context.Context, *journal.Reader) error { return nil }
			dir, err := f.launcher.layout.FindRunDir(id.RunID)
			if err != nil {
				t.Fatal(err)
			}
			writer, _, err := journal.TryRecover(dir)
			if err != nil {
				t.Fatal(err)
			}
			finished := time.Now()
			if err = writer.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase), Time: finished}); err != nil {
				t.Fatal(err)
			}
			if err = writer.Close(); err != nil {
				t.Fatal(err)
			}
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			finished = events[len(events)-1].Time
			ref := currentChildRef(t, f)
			if err = f.service.queue.SetChildState(t.Context(), ref.Child.Identity, triggerqueue.ChildStateUpdate{Expected: ref.Child.State, State: triggerqueue.ChildRunning}, finished); err != nil {
				t.Fatal(err)
			}
			ref = currentChildRef(t, f)
			state := triggerqueue.ChildFailed
			if phase == journal.PhaseEscalated {
				state = triggerqueue.ChildAwaitingHuman
			}
			result, err := (&childworkflow.WorkspaceCoordinator{Queue: f.service.queue}).CaptureResult(t.Context(), ref.Child, nil, childworkflow.TerminalResultInput{State: state, FinishedAt: finished, Summary: "Stopped original"})
			if err != nil {
				t.Fatal(err)
			}
			update := triggerqueue.ChildStateUpdate{Expected: ref.Child.State, State: state}
			if state.Terminal() {
				update.ResultRef = result.ResultRef
			}
			if err = f.service.queue.SetChildState(t.Context(), ref.Child.Identity, update, finished); err != nil {
				t.Fatal(err)
			}
			plan := []byte("retained next epoch plan")
			next, _, err := f.service.queue.BeginChildRestart(t.Context(), triggerqueue.ChildRestartRequest{Identity: ref.Child.Identity, RunID: "human-next", SourceRunID: id.RunID, SourceTerminalSeq: events[len(events)-1].Seq, SourceResultRef: result.ResultRef, Actor: "verified-human", Stage: "work", Plan: plan, PlanDigest: journal.Digest(plan)}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			ref = currentChildRef(t, f)
			c := requestInitialChildControl(t, f.service, ref)
			observed, _, err := f.service.cancelTypedQueueStart(t.Context(), c)
			if err != nil || observed.State != startcontrol.CancellationAlreadyTerminal {
				t.Fatal(observed, err)
			}
			current, err := f.service.queue.GetChild(t.Context(), ref.Child.Identity)
			if err != nil || current.ActiveRunID() != next.RunID || current.State != triggerqueue.ChildQueued || current.CancellationRequested || current.ResultRef != "" || !current.AcknowledgedAt.IsZero() {
				t.Fatal("old control changed new epoch", current, err)
			}
		})
	}
}
