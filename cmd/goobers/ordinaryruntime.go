package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/workflow"
)

type ordinaryRuntimeBuilder func(context.Context, startintent.Target, *engineRuntime) (startintent.Prepared, error)

func ordinaryRuntimeBuilderFor(layout instance.Layout, retainer *configgeneration.Retainer, build generationDefinitionBuilder) ordinaryRuntimeBuilder {
	return func(ctx context.Context, target startintent.Target, engine *engineRuntime) (startintent.Prepared, error) {
		if retainer == nil || build == nil {
			return startintent.Prepared{}, errors.New("ordinary runtime unavailable")
		}
		directory, lease, err := retainer.Store.Acquire(ctx, target.ConfigGeneration)
		if err != nil {
			return startintent.Prepared{}, err
		}
		definitions, err := loadPinnedStartDefinitions(layout.WithConfigDir(directory), build)
		if err == nil {
			definitions.EngineRuntime.adoptFrom(engine)
		}
		var entry localscheduler.WorkflowEntry
		if err == nil {
			entry, _, err = pinnedStartDefinition(definitions, target)
		}
		if err != nil {
			_ = lease.Release()
			return startintent.Prepared{}, err
		}
		return startintent.Prepared{Entry: entry, Release: func() { _ = lease.Release() }}, nil
	}
}

func loadPinnedStartDefinitions(layout instance.Layout, build generationDefinitionBuilder) (*schedulerDefinitions, error) {
	set, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		return nil, fmt.Errorf("load accepted execution generation: %w", err)
	}
	return build(layout, set, report)
}

func pinnedStartDefinition(definitions *schedulerDefinitions, target startintent.Target) (localscheduler.WorkflowEntry, executionGenerationRuntime, error) {
	key := localscheduler.WorkflowIdentity{Gaggle: target.Gaggle, Workflow: target.Workflow}
	runtime := executionGenerationRuntime{runner: definitions.Runners[key.Gaggle], machine: definitions.Machines[key], gooberDigest: definitions.GooberDigests[key], repoRef: definitions.RepoRefs[key]}
	if runtime.runner == nil || runtime.machine == nil || runtime.machine.Digest() != target.WorkflowDigest || runtime.gooberDigest != target.GooberDigest {
		return localscheduler.WorkflowEntry{}, runtime, errors.New("accepted runtime differs from workflow or Goober pins")
	}
	for _, entry := range definitions.Entries {
		if entry.Gaggle == key.Gaggle && entry.Workflow == key.Workflow {
			return entry, runtime, nil
		}
	}
	return localscheduler.WorkflowEntry{}, runtime, errors.New("accepted runtime workflow unavailable")
}

func buildPinnedStartMetadata(input schedulerDefinitionsInput, generation string, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, digests map[localscheduler.WorkflowIdentity]string) (eventPublicationSnapshot, generationDefinitionBuilder, error) {
	catalog, err := buildEventPublicationSnapshot(input.Definitions, generation, machines, digests)
	return catalog, schedulerGenerationBuilder(input), err
}
