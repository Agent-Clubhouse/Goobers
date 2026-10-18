package main

import (
	"sync"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// schedulerRuntime owns generation leases and the per-gaggle runtime graph.
// Runners, registries and worktree managers do not start background work during
// construction. Only the retainer needs closing, including on any later error.
// The caller drains dispatched work before closing a successful runtime.
type schedulerRuntime struct {
	generations     *configgeneration.Retainer
	definitions     *schedulerDefinitions
	legacyRunner    *runner.Runner
	legacyWorktrees *worktree.Manager
	providerQuota   *localscheduler.ProviderQuotaState
	runnerRegistry  *daemonRunnerRegistry
	interventions   *interventionDefinitionRegistry
	closeOnce       sync.Once
	closeErr        error
}

func openSchedulerRuntime(input schedulerDefinitionsInput) (*schedulerRuntime, error) {
	owned := &schedulerRuntime{}
	if err := owned.open(input); err != nil {
		return nil, err
	}
	return owned, nil
}

// open registers the generation retainer before any definitions can acquire a
// lease. Failed owners have released all leases and must not be reused.
func (owned *schedulerRuntime) open(input schedulerDefinitionsInput) (err error) {
	defer func() {
		if err != nil {
			_ = owned.Close()
		}
	}()
	// #712: shared with the Scheduler via SchedulerOptions below — see
	// schedulerSetup.ProviderQuota's doc comment for why a shared pointer,
	// not a Scheduler-owned field, is needed here.
	owned.providerQuota = localscheduler.NewProviderQuotaState()
	owned.runnerRegistry = newDaemonRunnerRegistry()
	owned.generations, err = newExecutionGenerationRetainer(input.Layout)
	if err != nil {
		return err
	}
	input.RunnerRegistry = owned.runnerRegistry
	input.ProviderQuota = owned.providerQuota
	input.Generations = []*configgeneration.Retainer{owned.generations}
	owned.definitions, err = buildSchedulerDefinitions(input)

	if err != nil {
		return err
	}
	owned.runnerRegistry.Replace(owned.definitions.Runners)
	reportStartupProgress(input.StartupProgress, "initializing retained legacy runtime")
	owned.legacyRunner, owned.legacyWorktrees, err = buildRetainedLegacyRunner(retainedLegacyRunnerInput{
		Layout:               input.Layout,
		Config:               input.Config,
		Definitions:          input.Definitions,
		Goobers:              owned.definitions.Goobers,
		InstructionsByGoober: owned.definitions.Instructions,
		InstructionFailures:  owned.definitions.InstructionFailures,
		Telemetry:            input.Telemetry,
		InstanceLog:          input.InstanceLog,
		SharedRegistry:       input.SharedRegistry,
		ProviderQuota:        owned.providerQuota,
		Watermarks:           input.Watermarks,
		TerminalNotifier:     input.TerminalNotifier,
		HarnessInfo:          owned.definitions.HarnessPreflight,
		CredentialStores:     input.CredentialStores,
	})
	if err != nil {
		return err
	}
	owned.interventions = newInterventionDefinitionRegistry(interventionDefinitions(owned.definitions, owned.legacyRunner))

	owned.runnerRegistry.setGenerationResolver(owned.definitions.GenerationResolver)
	return nil
}

func (owned *schedulerRuntime) Close() error {
	if owned == nil {
		return nil
	}
	owned.closeOnce.Do(func() {
		if owned.generations != nil {
			owned.closeErr = owned.generations.Close()
		}
	})
	return owned.closeErr
}
