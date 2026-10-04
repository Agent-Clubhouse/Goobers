package main

import (
	"context"
	"errors"
	"sync"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// childExecutorProvider constructs only isolated, custody-aware stage factories.
// Inputs come from queue custody and a freshly reconstructed immutable archive.
// Implementations must admit every pinned placement before returning factories.
type childExecutorProvider func(context.Context, childExecutionStart, preparedChildRuntime) (runner.ChildExecutionFactories, error)

func (s *daemonCredentialService) installQueuedChildren(setup *schedulerSetup, triggers *durableTriggerService, wg *sync.WaitGroup) error {
	if setup == nil || setup.RunnerRegistry == nil || s.children == nil || triggers == nil || triggers.queue == nil || triggers.dispatch == nil {
		return errors.New("daemon: child execution coordination is incomplete")
	}
	s.installParentRecovery(setup.RunnerRegistry)
	launcher := &queuedChildLauncher{layout: s.layout, config: s.config, queue: triggers.queue, authority: s.children,
		dispatch: triggers.dispatch, runners: setup.RunnerRegistry, wg: wg}
	launcher.build = s.isolatedChildBuilder(setup.ChildRuntime)
	launcher.result = launcher.captureTerminal
	launcher.reconcile = s.reconcileChildPodCustody
	triggers.children = launcher
	s.childCredentials = launcher.credentialCeiling
	setup.RunnerRegistry.setChildGenerationResolver(launcher.resolveGeneration)
	return nil
}

func (s *daemonCredentialService) isolatedChildBuilder(build childRuntimeBuilder) childRuntimeBuilder {
	return func(ctx context.Context, start childExecutionStart) (preparedChildRuntime, error) {
		if build == nil || s.childExecutors == nil {
			return preparedChildRuntime{}, &childStartDeferred{Reason: "isolated child stage backend unavailable"}
		}
		return isolatedChildRuntime(build, s.childExecutors)(ctx, start)
	}
}

func isolatedChildRuntime(build childRuntimeBuilder, factories childExecutorProvider) childRuntimeBuilder {
	return func(ctx context.Context, start childExecutionStart) (preparedChildRuntime, error) {
		runtime, err := build(ctx, start)
		if err != nil {
			return preparedChildRuntime{}, err
		}
		configured, err := bindChildRuntime(ctx, start, runtime, factories)
		if err != nil && runtime.release != nil {
			runtime.release()
		}
		return configured, err
	}
}

func bindChildRuntime(ctx context.Context, start childExecutionStart, runtime preparedChildRuntime, factories childExecutorProvider) (preparedChildRuntime, error) {
	if factories == nil {
		return preparedChildRuntime{}, &childStartDeferred{Reason: "isolated child stage backend unavailable"}
	}
	executors, err := factories(ctx, start, runtime)
	if err != nil {
		return preparedChildRuntime{}, err
	}
	id := journal.RunIdentity{RunID: start.Child.RunID, Gaggle: start.Envelope.Gaggle, Workflow: start.Envelope.Workflow,
		WorkflowDigest: start.Envelope.WorkflowDigest, GooberDigest: runtime.gooberDigest, ConfigGeneration: start.Envelope.ConfigGeneration, Child: &start.Lineage}
	runtime.runner, err = runtime.runner.ForChildExecution(id, executors)
	if err != nil {
		return preparedChildRuntime{}, err
	}
	// Local-runner probes are inapplicable once the exclusive stage dispatcher
	// admitted the exact retained placement. Parent limits remain in ReserveChild.
	runtime.entry.RequiredCapabilities = nil
	runtime.entry.PlacementRefusal, runtime.entry.HarnessRefusal = "", ""
	return runtime, nil
}
