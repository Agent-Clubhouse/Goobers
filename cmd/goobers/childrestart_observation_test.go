package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

type observedChildEpoch struct {
	childExecutionLauncher
	runs []string
}

func (l *observedChildEpoch) Result(_ context.Context, ref childExecutionRef) (childExecutionResult, error) {
	l.runs = append(l.runs, ref.runID())
	return childExecutionResult{}, nil
}

func TestChildDrainObservesCurrentEpochAndFencesLateOriginalResult(t *testing.T) {
	service, pinned, _ := humanChildCredentialFixture(t)
	launcher := &observedChildEpoch{childExecutionLauncher: &journalChildLauncher{}}
	triggers := &durableTriggerService{queue: service.childQueue, dispatch: newDaemonTriggerService(), observeChild: acceptedChildObserver(service.layout), children: launcher}
	if err := triggers.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(launcher.runs) != 1 || launcher.runs[0] != pinned.identity.RunID {
		t.Fatal("wrong execution observed", launcher.runs)
	}
	ref, err := retainedChildExecutionRef(t.Context(), service.childQueue, pinned.identity, true)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Child.State != triggerqueue.ChildRunning {
		t.Fatal(ref.Child.State)
	}
	original := ref
	original.Execution = nil
	original.Lineage.ExecutionEpoch = 0
	original.Lineage.PriorResultRef = ""
	original.Lineage.RestartDigest = ""
	if err = triggers.recordChildResult(t.Context(), original, childExecutionResult{State: triggerqueue.ChildFailed, ResultRef: ref.Execution.SourceResultRef}, true); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatal("late original result overwritten epoch", err)
	}
	current, err := service.childQueue.GetChild(t.Context(), ref.Child.Identity)
	if err != nil || current.ActiveRunID() != pinned.identity.RunID || current.State != triggerqueue.ChildRunning || current.ResultRef != "" {
		t.Fatal(current, err)
	}
}

func TestQueuedChildEpochCancellationSealsOwnResultWithoutNewStart(t *testing.T) {
	service, pinned, _ := humanChildCredentialFixture(t)
	ref, err := retainedChildExecutionRef(t.Context(), service.childQueue, pinned.identity, true)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := service.layout.FindRunDir(ref.runID())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	} // simulate accepted-before-journal crash in this temporary fixture
	at := time.Now()
	if err = service.childQueue.FenceChildParent(t.Context(), ref.Child.Identity.ChildParent, "human", at); err != nil {
		t.Fatal(err)
	}
	launcher := &queuedChildLauncher{layout: service.layout, queue: service.childQueue, runners: newDaemonRunnerRegistry(), restart: func(context.Context, childExecutionRef) error { t.Error("cancelled epoch restarted"); return nil }}
	launcher.result = launcher.captureTerminal
	triggers := &durableTriggerService{queue: service.childQueue, dispatch: newDaemonTriggerService(), observeChild: acceptedChildObserver(service.layout), children: launcher}
	if err = triggers.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := service.childQueue.GetChild(t.Context(), ref.Child.Identity)
	if err != nil || current.State != triggerqueue.ChildCancelled || current.ResultRef == ref.Execution.SourceResultRef || current.ResultRef == "" {
		t.Fatal(current, err)
	}
	original, err := service.childQueue.ChildExecutionResult(t.Context(), ref.Child.Identity, ref.Child.RunID)
	if err != nil || original.ReceiptDigest != ref.Execution.SourceResultRef {
		t.Fatal("prior result changed", err)
	}
}
