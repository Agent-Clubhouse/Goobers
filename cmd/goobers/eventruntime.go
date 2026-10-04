package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/eventexecution"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/startintent"
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
	definitions, err := loadPinnedStartDefinitions(layout, build)
	if err != nil {
		return preparedEventRuntime{}, err
	}
	entry, execution, err := pinnedStartDefinition(definitions, startintent.Target{Gaggle: start.Gaggle, Workflow: start.Workflow, ConfigGeneration: start.ConfigGeneration, WorkflowDigest: start.WorkflowDigest, GooberDigest: start.GooberDigest})
	if err != nil {
		return preparedEventRuntime{}, err
	}
	runtime := preparedEventRuntime{Prepared: eventexecution.Prepared{Entry: entry}, executionGenerationRuntime: execution}
	if runtime.Entry.Workflow == "" || featureDriver(runtime.Entry.Starter) != "runner.local" {
		return preparedEventRuntime{}, errors.New("pinned event execution requires the local runner; engine consumer input transport is unavailable")
	}
	return runtime, nil
}
