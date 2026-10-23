package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/configgeneration"
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

// All execution paths rebuild through the same captured-definition compiler.
func bindGenerationRuntimes(definitions *schedulerDefinitions, input schedulerDefinitionsInput, layout instance.Layout, generation string) *schedulerDefinitions {
	build := schedulerGenerationBuilder(input)
	retainer := firstGenerationRetainer(input.Generations)
	definitions.ExecutionGeneration = generation
	for i := range definitions.Entries {
		definitions.Entries[i].ConfigGeneration = generation
	}
	definitions.OrdinaryRuntime = ordinaryRuntimeBuilderFor(layout, retainer, build)
	childInput := input
	childInput.TrustedLocalChildRuntime = trustedLocalChildren(input.Config)
	definitions.ChildRuntime = childRuntimeBuilderFor(layout, retainer, input.Config, schedulerGenerationBuilder(childInput))
	definitions.GenerationResolver = generationResolverFor(layout, retainer, build)
	return definitions
}

func newExecutionGenerationRetainer(layout instance.Layout) (*configgeneration.Retainer, error) {
	store, err := executionGenerationStore(layout)
	if err != nil {
		return nil, err
	}
	return &configgeneration.Retainer{Store: store, DurablePins: executionGenerationPinsReader(instance.NewLayout(layout.Root))}, nil
}

// One cursor per store/retainer observes new journal appends without rereading
// history on every Keep. It also invalidates on journal replacement/compaction.
func executionGenerationPinsReader(layout instance.Layout) func(context.Context) (map[string]bool, error) {
	applied := appliedExecutionGenerationReader(layout)
	return func(ctx context.Context) (map[string]bool, error) {
		generation, err := applied(ctx)
		if err != nil {
			return nil, err
		}
		return retainedExecutionGenerationPinsWithApplied(ctx, layout, generation)
	}
}

// A terminal journal can still be recoverable. Protect every retained identity;
// journal retention, rather than an active-run registry, ends its ownership.
func retainedExecutionGenerationPinsWithApplied(ctx context.Context, layout instance.Layout, applied string) (map[string]bool, error) {
	roots, err := layout.RunDirsContext(ctx)
	if err != nil {
		return nil, err
	}
	pins, err := retainedOrdinaryGenerationPins(ctx, layout)
	if err != nil {
		return nil, err
	}
	if applied != "" {
		pins[applied] = true
	}
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
	before, err := runtimewiring.ConfigDirectoryDigest(layout.ConfigDir())
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
	after, err := runtimewiring.ConfigDirectoryDigest(directory)
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

// generationDefinitionBuilder compiles runtime definitions from the retained
// tree of the named admitted generation.
type generationDefinitionBuilder func(instance.Layout, string, *instance.ConfigSet, *validate.Report) (*schedulerDefinitions, error)

func generationResolverFor(layout instance.Layout, retainer *configgeneration.Retainer, build generationDefinitionBuilder) executionGenerationResolver {
	if retainer == nil {
		return nil
	}
	return func(ctx context.Context, identity journal.RunIdentity) (executionGenerationRuntime, error) {
		// Generated runs require retained source plus parent-generation custody;
		// their display names must never select a mutable catalog definition.
		if identity.Child != nil {
			return executionGenerationRuntime{}, errors.New("generated child recovery requires retained proposal resolver")
		}
		directory, lease, err := retainer.Store.Acquire(ctx, identity.ConfigGeneration)
		if err != nil {
			return executionGenerationRuntime{}, err
		}
		defer func() { _ = lease.Release() }()
		set, report, err := loadConfigDirectory(directory)
		if err != nil {
			return executionGenerationRuntime{}, fmt.Errorf("load pinned execution definitions: %w (%s)", err, runtimewiring.ValidationIssueSummary(report))
		}
		definitions, err := build(instance.NewLayout(layout.Root).WithConfigDir(directory), identity.ConfigGeneration, set, report)
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
	reconcile := r.reconcileContained
	if identity.Child != nil {
		resolve = r.resolveChildGeneration
	}
	r.mu.RUnlock()
	if resolve == nil {
		return executionGenerationRuntime{}, errors.New("no resolver for pinned execution generation")
	}
	if identity.Child == nil && reconcile != nil {
		if err := reconcile(ctx, identity); err != nil {
			return executionGenerationRuntime{}, err
		}
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
		return credentialPlaneDefinitions{}, nil, fmt.Errorf("load pinned credential definitions: %w (%s)", err, runtimewiring.ValidationIssueSummary(report))
	}
	return credentialPlaneDefinitionsFromSet(set), release, nil
}

func firstGenerationRetainer(retainers []*configgeneration.Retainer) *configgeneration.Retainer {
	if len(retainers) == 0 {
		return nil
	}
	return retainers[0]
}

// schedulerExecutionGeneration binds a pinned rebuild to the run's admitted
// generation. Re-retaining its extracted tree is not byte-stable (Windows
// reads back different mode bits), and a fresh identity would make every
// later CLI stage of the run fail config_generation_mismatch (#5445).
func schedulerExecutionGeneration(input schedulerDefinitionsInput) (instance.Layout, string, error) {
	if input.PinnedGeneration != "" {
		return input.Layout, input.PinnedGeneration, nil
	}
	return retainOptionalExecutionGeneration(input.Layout, input.Generations)
}

func retainOptionalExecutionGeneration(layout instance.Layout, retainers []*configgeneration.Retainer) (instance.Layout, string, error) {
	retainer := firstGenerationRetainer(retainers)
	if retainer == nil {
		return layout, "", nil
	}
	return retainExecutionGeneration(context.Background(), layout, retainer)
}
