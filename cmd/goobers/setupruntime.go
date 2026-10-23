package main

import (
	"context"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// schedulerRuntime owns generation leases and the per-gaggle runtime graph.
// Runners, registries and worktree managers do not start background work during
// construction. Only the retainer needs closing, including on any later error.
// The caller drains dispatched work before closing a successful runtime.
type schedulerRuntime struct {
	*scheduling.Runtime
	definitions     *schedulerDefinitions
	legacyRunner    *runner.Runner
	legacyWorktrees *worktree.Manager
	providerQuota   *localscheduler.ProviderQuotaState
	runnerRegistry  *daemonRunnerRegistry
	interventions   *interventionDefinitionRegistry
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
func (owned *schedulerRuntime) open(input schedulerDefinitionsInput) error {
	owned.Runtime = &scheduling.Runtime{}
	owned.providerQuota = localscheduler.NewProviderQuotaState()
	owned.runnerRegistry = newDaemonRunnerRegistry()
	return owned.Open(scheduling.RuntimeDependencies{
		NewRetainer: func() (*configgeneration.Retainer, error) { return newExecutionGenerationRetainer(input.Layout) },
		Bind: func(retainer *configgeneration.Retainer) {
			input.RunnerRegistry = owned.runnerRegistry
			input.ProviderQuota = owned.providerQuota
			input.Generations = []*configgeneration.Retainer{retainer}
		},
		ExplicitRecovery:  input.RecoveryGeneration != "",
		RecoverAtCapacity: func() bool { return input.RecoverAtCapacity },
		Recover: func() (string, error) {
			var digest string
			var err error
			input, digest, err = recoverStartupGeneration(context.Background(), input, owned.Generations)
			return digest, err
		},
		Build: func() error { var err error; owned.definitions, err = buildSchedulerDefinitions(input); return err },
		Install: func() error {
			owned.runnerRegistry.Replace(owned.definitions.Runners)
			reportStartupProgress(input.StartupProgress, "initializing retained legacy runtime")
			var err error
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
		},
		CapacityMessage: generationCapacityMessage,
		Progress:        func(message string) { reportStartupProgress(input.StartupProgress, message) },
	})
}
func (owned *schedulerRuntime) Close() error {
	if owned == nil {
		return nil
	}
	return owned.Runtime.Close()
}
