package main

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/workflow"
)

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
	set, close, err := generationService().PinnedConfig(ctx, layout, generation)
	if err != nil {
		return credentialPlaneDefinitions{}, nil, err
	}
	return credentialPlaneDefinitionsFromSet(set), close, nil
}

// schedulerExecutionGeneration binds a pinned rebuild to the run's admitted
// generation. Re-retaining its extracted tree is not byte-stable (Windows
// reads back different mode bits), and a fresh identity would make every
// later CLI stage of the run fail config_generation_mismatch (#5445).
func schedulerExecutionGeneration(input schedulerDefinitionsInput) (instance.Layout, string, error) {
	if input.PinnedGeneration != "" {
		return input.Layout, input.PinnedGeneration, nil
	}
	return retainOptionalExecutionGeneration(input.Layout, input.Generations, workflowSourceProvenance(input.Config, input.WorkflowSourceRevision))
}

func workflowSourceProvenance(cfg *instance.Config, revision string) *journal.WorkflowSourceProvenance {
	if cfg == nil || cfg.WorkflowSource == nil || cfg.WorkflowSource.Kind != instance.WorkflowSourceKindGit || cfg.WorkflowSource.URL == "" || revision == "" {
		return nil
	}
	return &journal.WorkflowSourceProvenance{
		URL:    cfg.WorkflowSource.URL,
		Dir:    cfg.WorkflowSource.Directory(),
		Commit: revision,
	}
}

func generationService() scheduling.Generations {
	return scheduling.Generations{OrdinaryPins: retainedOrdinaryGenerationPins, Digest: runtimewiring.ConfigDirectoryDigest, Load: loadConfigDirectory, IssueSummary: runtimewiring.ValidationIssueSummary}
}

var executionGenerationStore = scheduling.ExecutionGenerationStore
var verifyStageExecutionGeneration = scheduling.VerifyStageExecutionGeneration
var firstGenerationRetainer = scheduling.FirstGenerationRetainer

func lastAppliedWorkflowSource(layout instance.Layout) (*journal.WorkflowSourceProvenance, error) {
	latest := journal.NewInstanceLatest(layout.SchedulerDir(), journal.IsConfigGenerationAuthority)
	event, found, err := latest.Read(context.Background())
	if err != nil || !found {
		return nil, err
	}
	fields, ok := event.Runner["workflowSource"].(map[string]any)
	if !ok {
		return nil, nil
	}
	source := &journal.WorkflowSourceProvenance{}
	source.URL, _ = fields["url"].(string)
	source.Dir, _ = fields["dir"].(string)
	source.Commit, _ = fields["commit"].(string)
	if source.URL == "" || source.Dir == "" || source.Commit == "" {
		return nil, errors.New("latest config generation has incomplete workflow source provenance")
	}
	return source, nil
}

// startupWorkflowSource returns the journaled provenance only when the latest
// authoritative event applied the same generation, so a restart annotation
// never attributes a source to a different generation.
func startupWorkflowSource(layout instance.Layout, generation string) (*journal.WorkflowSourceProvenance, error) {
	latest := journal.NewInstanceLatest(layout.SchedulerDir(), journal.IsConfigGenerationAuthority)
	event, found, err := latest.Read(context.Background())
	if err != nil || !found {
		return nil, err
	}
	if applied, _ := event.Runner["configGeneration"].(string); applied != generation {
		return nil, nil
	}
	return lastAppliedWorkflowSource(layout)
}

func appliedWorkflowSourceRevision(layout instance.Layout, cfg *instance.Config) (string, error) {
	if cfg == nil || cfg.WorkflowSource == nil || cfg.WorkflowSource.Kind != instance.WorkflowSourceKindGit || cfg.WorkflowSource.URL == "" {
		return "", nil
	}
	source, err := lastAppliedWorkflowSource(layout)
	if err != nil {
		return "", err
	}
	if source == nil || source.URL != cfg.WorkflowSource.URL || source.Dir != cfg.WorkflowSource.Directory() {
		return "", nil
	}
	return source.Commit, nil
}

func newExecutionGenerationRetainer(layout instance.Layout) (*configgeneration.Retainer, error) {
	return generationService().NewRetainer(layout)
}

func retainOptionalExecutionGeneration(layout instance.Layout, retainers []*configgeneration.Retainer, source *journal.WorkflowSourceProvenance) (instance.Layout, string, error) {
	return generationService().RetainOptional(layout, retainers, source)
}

func generationResolverFor(layout instance.Layout, retainer *configgeneration.Retainer, build generationDefinitionBuilder) executionGenerationResolver {
	resolve := generationService().Resolver(layout, retainer, func(l instance.Layout, generation string, set *instance.ConfigSet, report *validate.Report, identity journal.RunIdentity) (scheduling.GenerationRuntime, error) {
		definitions, err := build(l, generation, set, report)
		if err != nil {
			return scheduling.GenerationRuntime{}, err
		}
		key := localscheduler.WorkflowIdentity{Gaggle: identity.Gaggle, Workflow: identity.Workflow}
		return scheduling.GenerationRuntime{Runner: definitions.Runners[identity.Gaggle], Machine: definitions.Machines[key], GooberDigest: definitions.GooberDigests[key], RepoRef: definitions.RepoRefs[key]}, nil
	})
	if resolve == nil {
		return nil
	}
	return func(ctx context.Context, identity journal.RunIdentity) (executionGenerationRuntime, error) {
		r, err := resolve(ctx, identity)
		return executionGenerationRuntime{runner: r.Runner, machine: r.Machine, gooberDigest: r.GooberDigest, repoRef: r.RepoRef}, err
	}
}
