package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/eventexecution"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

type eventRuntimeBuilder func(context.Context, eventing.StartEnvelope) (preparedEventRuntime, error)
type preparedEventRuntime struct {
	eventexecution.Prepared
	executionGenerationRuntime
}

func eventRuntimeBuilderFor(layout instance.Layout, retainer *configgeneration.Retainer, build generationDefinitionBuilder) eventRuntimeBuilder {
	return func(ctx context.Context, start eventing.StartEnvelope) (preparedEventRuntime, error) {
		if retainer == nil || build == nil {
			return preparedEventRuntime{}, errors.New("event runtime unavailable")
		}
		directory, lease, err := retainer.Store.Acquire(ctx, start.ConfigGeneration)
		if err != nil {
			return preparedEventRuntime{}, err
		}
		runtime, err := buildEventRuntime(layout.WithConfigDir(directory), build, start)
		if err != nil {
			_ = lease.Release()
			return preparedEventRuntime{}, err
		}
		runtime.Release = func() { _ = lease.Release() }
		return runtime, nil
	}
}

func buildEventRuntime(layout instance.Layout, build generationDefinitionBuilder, start eventing.StartEnvelope) (preparedEventRuntime, error) {
	set, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		return preparedEventRuntime{}, fmt.Errorf("load event generation: %w", err)
	}
	definitions, err := build(layout, set, report)
	if err != nil {
		return preparedEventRuntime{}, err
	}
	key := localscheduler.WorkflowIdentity{Gaggle: start.Gaggle, Workflow: start.Workflow}
	runtime := preparedEventRuntime{executionGenerationRuntime: executionGenerationRuntime{runner: definitions.Runners[key.Gaggle], machine: definitions.Machines[key], gooberDigest: definitions.GooberDigests[key], repoRef: definitions.RepoRefs[key]}}
	if runtime.runner == nil || runtime.machine == nil || runtime.machine.Digest() != start.WorkflowDigest || runtime.gooberDigest != start.GooberDigest {
		return preparedEventRuntime{}, errors.New("event runtime differs from accepted workflow or Goober pins")
	}
	for _, entry := range definitions.Entries {
		if entry.Gaggle == key.Gaggle && entry.Workflow == key.Workflow {
			runtime.Entry = entry
			break
		}
	}
	if runtime.Entry.Workflow == "" || featureDriver(runtime.Entry.Starter) != "runner.local" {
		return preparedEventRuntime{}, errors.New("pinned event execution requires the local runner; engine consumer input transport is unavailable")
	}
	return runtime, nil
}
