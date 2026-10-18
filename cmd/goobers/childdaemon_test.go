package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildDaemonInstallsRecoveryCredentialsAndDefersMissingBackend(t *testing.T) {
	f := newHandoffDaemonFixture(t)
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	dispatch := newDaemonTriggerService()
	dispatch.AttachScheduler(localscheduler.New(nil, log))
	triggers := &durableTriggerService{queue: f.queue, dispatch: dispatch, observeChild: acceptedChildObserver(f.service.layout)}
	setup := &schedulerSetup{RunnerRegistry: newDaemonRunnerRegistry(), ChildRuntime: func(context.Context, childExecutionStart) (preparedChildRuntime, error) {
		t.Fatal("missing backend built a local runtime")
		return preparedChildRuntime{}, nil
	}}
	var wg sync.WaitGroup
	if err := f.service.installQueuedChildren(setup, triggers, &wg); err != nil {
		t.Fatal(err)
	}
	if triggers.children == nil || f.service.childCredentials == nil || setup.RunnerRegistry.resolveChildGeneration == nil {
		t.Fatal("daemon left child coordination disconnected")
	}
	if err := triggers.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	receipt, err := f.queue.ChildStart(t.Context(), f.child.Identity)
	if err != nil || receipt.State != triggerqueue.Accepted {
		t.Fatalf("missing backend lost durable intent: %+v %v", receipt, err)
	}
	if _, err := f.service.layout.FindRunDir(f.child.RunID); err == nil {
		t.Fatal("missing backend published child journal")
	}
}

func isolatedChildLaunchFixture(t *testing.T) (*actualChildFixture, *launchDeterministic) {
	t.Helper()
	f := actualChildLaunchFixture(t)
	build := f.launcher.build
	isolated := &launchDeterministic{}
	f.launcher.build = isolatedChildRuntime(build, func(_ context.Context, start childExecutionStart, runtime preparedChildRuntime) (runner.ChildExecutionFactories, error) {
		if start.Proposal == nil || start.Lineage.SourceDigest != start.Proposal.SourceDigest || runtime.machine.Digest() != start.Envelope.WorkflowDigest {
			t.Fatal("factory did not receive exact retained admission")
		}
		return runner.ChildExecutionFactories{
			NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
				return isolated, nil
			},
			NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
				return nil, errors.New("unused goober")
			},
		}, nil
	})
	return f, isolated
}

func TestChildDaemonRuntimeRoutesReopenedRecovery(t *testing.T) {
	f, isolated := isolatedChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	recovered, err := f.launcher.resolveGeneration(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := recovered.runner.Resume(t.Context(), runner.ResumeInput{RunID: id.RunID, Machine: recovered.machine, GooberDigest: recovered.gooberDigest, RepoRef: recovered.repoRef})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatal(result, err)
	}
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if isolated.calls.Load() != 1 || f.executor.calls.Load() != 0 {
		t.Fatal("generated execution selected or replayed ordinary factory")
	}
}

func TestChildDaemonRuntimeRoutesActualDrain(t *testing.T) {
	f, isolated := isolatedChildLaunchFixture(t)
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wg.Wait()
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if isolated.calls.Load() != 1 || f.executor.calls.Load() != 0 {
		t.Fatal("child selected or replayed ordinary factory")
	}
}
