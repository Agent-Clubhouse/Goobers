package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

func executionGenerationStore(layout instance.Layout) (configgeneration.Store, error) {
	blobs, err := blobstore.NewDir(filepath.Join(layout.BlobStoreDir(), "config-generations"))
	if err != nil {
		return configgeneration.Store{}, err
	}
	return configgeneration.Store{Root: filepath.Join(layout.Root, "config-generations"), Blobs: blobs}, nil
}

func newExecutionGenerationRetainer(layout instance.Layout) (*configgeneration.Retainer, error) {
	store, err := executionGenerationStore(layout)
	if err != nil {
		return nil, err
	}
	return &configgeneration.Retainer{Store: store, DurablePins: func(ctx context.Context) (map[string]bool, error) {
		return retainedExecutionGenerationPins(ctx, instance.NewLayout(layout.Root))
	}}, nil
}

// A terminal journal can still be recoverable. Protect every retained identity;
// journal retention, rather than an active-run registry, ends its ownership.
func retainedExecutionGenerationPins(ctx context.Context, layout instance.Layout) (map[string]bool, error) {
	roots, err := layout.RunDirsContext(ctx)
	if err != nil {
		return nil, err
	}
	pins := make(map[string]bool)
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !entry.IsDir() {
				continue
			}
			reader, err := journal.OpenRead(filepath.Join(root, entry.Name()))
			if errors.Is(err, journal.ErrNotRunDirectory) {
				continue
			}
			if err != nil {
				return nil, err
			}
			identity, err := reader.Identity()
			if err != nil {
				return nil, fmt.Errorf("read config generation owner %q: %w", entry.Name(), err)
			}
			if identity.ConfigGeneration != "" {
				pins[identity.ConfigGeneration] = true
			}
		}
	}
	return pins, nil
}

func retainExecutionGeneration(ctx context.Context, layout instance.Layout, retainer *configgeneration.Retainer) (instance.Layout, string, error) {
	before, err := configDirectoryDigest(layout.ConfigDir())
	if err != nil {
		return layout, "", err
	}
	owner, err := layout.EnsureIdentity(ctx)
	if err != nil {
		return layout, "", err
	}
	data, generation, err := configgeneration.CaptureForInstance(ctx, layout.ConfigDir(), owner)
	if err != nil {
		return layout, "", err
	}
	directory, err := retainer.Keep(ctx, data, generation)
	if err != nil {
		return layout, "", err
	}
	after, err := configDirectoryDigest(directory)
	if err != nil {
		return layout, "", err
	}
	if before != after {
		return layout, "", errors.New("config changed while retaining execution generation")
	}
	return layout.WithConfigDir(directory), generation, nil
}

func verifyStageExecutionGeneration(layout instance.Layout) error {
	expected := os.Getenv(executor.ConfigGenerationEnvVar)
	if expected == "" {
		return nil
	}
	if os.Getenv(executor.ConfigDirectoryEnvVar) == "" {
		return errors.New("run config generation has no immutable execution directory")
	}
	return configgeneration.VerifyDirectory(context.Background(), layout.ConfigDir(), expected, os.Getenv(executor.InstanceIDEnvVar))
}

type executionGenerationRuntime struct {
	runner       *runner.Runner
	machine      *workflow.Machine
	gooberDigest string
	repoRef      apiv1.RepoRef
}

type executionGenerationResolver func(context.Context, journal.RunIdentity) (executionGenerationRuntime, error)

type generationDefinitionBuilder func(instance.Layout, *instance.ConfigSet, *validate.Report) (*schedulerDefinitions, error)

func generationResolverFor(layout instance.Layout, retainer *configgeneration.Retainer, build generationDefinitionBuilder) executionGenerationResolver {
	if retainer == nil {
		return nil
	}
	return func(ctx context.Context, identity journal.RunIdentity) (executionGenerationRuntime, error) {
		directory, lease, err := retainer.Store.Acquire(ctx, identity.ConfigGeneration)
		if err != nil {
			return executionGenerationRuntime{}, err
		}
		defer func() { _ = lease.Release() }()
		set, report, err := loadConfigDirectory(directory)
		if err != nil {
			return executionGenerationRuntime{}, fmt.Errorf("load pinned execution definitions: %w (%s)", err, validationIssueSummary(report))
		}
		definitions, err := build(instance.NewLayout(layout.Root).WithConfigDir(directory), set, report)
		if err != nil {
			return executionGenerationRuntime{}, err
		}
		key := localscheduler.WorkflowIdentity{Gaggle: identity.Gaggle, Workflow: identity.Workflow}
		runtime := executionGenerationRuntime{runner: definitions.Runners[identity.Gaggle], machine: definitions.Machines[key], gooberDigest: definitions.GooberDigests[key], repoRef: definitions.RepoRefs[key]}
		if runtime.runner == nil || runtime.machine == nil {
			return executionGenerationRuntime{}, errors.New("pinned execution generation does not contain the run workflow")
		}
		if runtime.machine.Digest() != identity.WorkflowDigest || runtime.gooberDigest != identity.GooberDigest {
			return executionGenerationRuntime{}, errors.New("pinned execution generation does not match durable workflow/goober identity")
		}
		return runtime, nil
	}
}

func (r *daemonRunnerRegistry) setGenerationResolver(resolve executionGenerationResolver) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.resolveGeneration = resolve
	r.mu.Unlock()
}

func (r *daemonRunnerRegistry) executionGeneration(ctx context.Context, identity journal.RunIdentity) (executionGenerationRuntime, error) {
	if r == nil {
		return executionGenerationRuntime{}, errors.New("no registry for pinned execution generation")
	}
	r.mu.RLock()
	resolve := r.resolveGeneration
	r.mu.RUnlock()
	if resolve == nil {
		return executionGenerationRuntime{}, errors.New("no resolver for pinned execution generation")
	}
	return resolve(ctx, identity)
}

func pinnedCredentialDefinitions(ctx context.Context, layout instance.Layout, generation string) (credentialPlaneDefinitions, func(), error) {
	store, err := executionGenerationStore(layout)
	if err != nil {
		return credentialPlaneDefinitions{}, nil, err
	}
	directory, lease, err := store.Acquire(ctx, generation)
	if err != nil {
		return credentialPlaneDefinitions{}, nil, err
	}
	release := func() { _ = lease.Release() }
	set, report, err := loadConfigDirectory(directory)
	if err != nil {
		release()
		return credentialPlaneDefinitions{}, nil, fmt.Errorf("load pinned credential definitions: %w (%s)", err, validationIssueSummary(report))
	}
	return credentialPlaneDefinitionsFromSet(set), release, nil
}

func firstGenerationRetainer(retainers []*configgeneration.Retainer) *configgeneration.Retainer {
	if len(retainers) == 0 {
		return nil
	}
	return retainers[0]
}
func retainOptionalExecutionGeneration(layout instance.Layout, retainers []*configgeneration.Retainer) (instance.Layout, string, error) {
	retainer := firstGenerationRetainer(retainers)
	if retainer == nil {
		return layout, "", nil
	}
	return retainExecutionGeneration(context.Background(), layout, retainer)
}

func (s *runInterventionService) interventionExecution(identity journal.RunIdentity, definitions interventionDefinitionSet, fallback *runner.Runner) (executionGenerationRuntime, error) {
	if identity.ConfigGeneration != "" {
		pinned, err := s.runnerRegistry.executionGeneration(context.Background(), identity)
		if err != nil {
			return executionGenerationRuntime{}, interventionConflict("config_generation_unavailable", err.Error())
		}
		return pinned, nil
	}
	key := localscheduler.WorkflowIdentity{Gaggle: identity.Gaggle, Workflow: identity.Workflow}
	machine := definitions.machines[key]
	if machine == nil {
		return executionGenerationRuntime{}, interventionConflict("workflow_unavailable", fmt.Sprintf("workflow %q for run %q is no longer available", identity.Workflow, identity.RunID))
	}
	return executionGenerationRuntime{runner: fallback, machine: machine, gooberDigest: definitions.gooberDigests[key], repoRef: definitions.repoRefs[key]}, nil
}

func pinnedDirectEngineInput(ctx context.Context, layout instance.Layout, cfg *instance.Config, gaggle, workflowName, dedupe string, liveJournal bool) (engine.RunInput, func(), error) {
	owner, err := layout.EnsureIdentity(ctx)
	if err != nil {
		return engine.RunInput{}, nil, err
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		return engine.RunInput{}, nil, err
	}
	store.DurablePins = func(ctx context.Context) (map[string]bool, error) {
		return retainedExecutionGenerationPins(ctx, instance.NewLayout(layout.Root))
	}
	archive, generation, err := configgeneration.CaptureForInstance(ctx, layout.ConfigDir(), owner)
	if err != nil {
		return engine.RunInput{}, nil, err
	}
	directory, lease, err := store.KeepExternallyOwned(ctx, archive, generation)
	if err != nil {
		return engine.RunInput{}, nil, err
	}
	release := func() { _ = lease.Release() }
	input, err := engineInputFromGeneration(directory, cfg, gaggle, workflowName, dedupe, owner, generation, liveJournal)
	if err != nil {
		release()
		return engine.RunInput{}, nil, err
	}
	return input, release, nil
}
func engineInputFromGeneration(directory string, cfg *instance.Config, gaggle, workflowName, dedupe, owner, generation string, liveJournal bool) (engine.RunInput, error) {
	set, report, err := loadConfigDirectory(directory)
	if err != nil {
		return engine.RunInput{}, fmt.Errorf("load pinned engine definitions: %w (%s)", err, validationIssueSummary(report))
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
	registry, project, err := bootstrap.RegisterGaggleWorkflows(set, gaggle)
	if err != nil {
		return engine.RunInput{}, err
	}
	definition, found := registry.Latest(workflowName)
	if !found {
		return engine.RunInput{}, fmt.Errorf("pinned workflow %q is unavailable", workflowName)
	}
	spec, err := engineRunSpec(engineRunRequest{configGeneration: generation, instanceID: owner, cfg: cfg, set: set, gaggle: gaggle, dedupeKey: dedupe, project: project, def: definition, liveJournal: liveJournal})
	if err != nil {
		return engine.RunInput{}, err
	}
	return registry.StartInput(workflowName, spec)
}
