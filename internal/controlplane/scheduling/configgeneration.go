package scheduling

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

// Generations owns retention and verification; compiling definitions stays with
// the daemon, supplied through the same captured-definition builder.
type Generations struct {
	OrdinaryPins func(context.Context, instance.Layout) (map[string]bool, error)
	Digest       func(string) (string, error)
	Load         func(string) (*instance.ConfigSet, *validate.Report, error)
	IssueSummary func(*validate.Report) string
}

// GenerationRuntime is the compiled runtime selected for one retained run.
type GenerationRuntime struct {
	Runner       *runner.Runner
	Machine      *workflow.Machine
	GooberDigest string
	RepoRef      apiv1.RepoRef
}

// GenerationResolver reconstructs a run from its admitted generation.
type GenerationResolver func(context.Context, journal.RunIdentity) (GenerationRuntime, error)

// GenerationBuilder compiles and selects a runtime from a retained config tree.
type GenerationBuilder func(instance.Layout, string, *instance.ConfigSet, *validate.Report, journal.RunIdentity) (GenerationRuntime, error)

// ExecutionGenerationStore opens the instance execution-generation archive.
func ExecutionGenerationStore(layout instance.Layout) (configgeneration.Store, error) {
	blobs, err := blobstore.NewDir(filepath.Join(layout.BlobStoreDir(), "config-generations"))
	if err != nil {
		return configgeneration.Store{}, err
	}
	return configgeneration.Store{Root: filepath.Join(layout.Root, "config-generations"), Blobs: blobs}, nil
}

// NewRetainer creates a generation owner with durable journal pins.
func (g Generations) NewRetainer(layout instance.Layout) (*configgeneration.Retainer, error) {
	store, err := ExecutionGenerationStore(layout)
	if err != nil {
		return nil, err
	}
	return &configgeneration.Retainer{Store: store, DurablePins: g.PinsReader(instance.NewLayout(layout.Root))}, nil
}

// PinsReader incrementally reads the applied generation and retained owners.
//
// One cursor per store/retainer observes new journal appends without rereading
// history on every Keep. It also invalidates on journal replacement/compaction.
func (g Generations) PinsReader(layout instance.Layout) func(context.Context) (map[string]bool, error) {
	applied := AppliedExecutionGenerationReader(layout)
	return func(ctx context.Context) (map[string]bool, error) {
		generation, err := applied(ctx)
		if err != nil {
			return nil, err
		}
		return g.RetainedPins(ctx, layout, generation)
	}
}

// RetainedPins protects generations referenced by retained journals and accepted work.
//
// A terminal journal can still be recoverable. Protect every retained identity;
// journal retention, rather than an active-run registry, ends its ownership.
func (g Generations) RetainedPins(ctx context.Context, layout instance.Layout, applied string) (map[string]bool, error) {
	roots, err := layout.RunDirsContext(ctx)
	if err != nil {
		return nil, err
	}
	pins, err := g.OrdinaryPins(ctx, layout)
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

// Retain captures and verifies an immutable execution configuration.
func (g Generations) Retain(ctx context.Context, layout instance.Layout, retainer *configgeneration.Retainer) (instance.Layout, string, error) {
	before, err := g.Digest(layout.ConfigDir())
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
	after, err := g.Digest(directory)
	if err != nil {
		return layout, "", err
	}
	if before != after {
		return layout, "", errors.New("config changed while retaining execution generation")
	}
	return layout.WithConfigDir(directory), generation, nil
}

// VerifyStageExecutionGeneration checks a stage against its pinned directory and owner.
func VerifyStageExecutionGeneration(layout instance.Layout) error {
	expected := os.Getenv(executor.ConfigGenerationEnvVar)
	if expected == "" {
		return nil
	}
	if os.Getenv(executor.ConfigDirectoryEnvVar) == "" {
		return errors.New("run config generation has no immutable execution directory")
	}
	return configgeneration.VerifyDirectory(context.Background(), layout.ConfigDir(), expected, os.Getenv(executor.InstanceIDEnvVar))
}

// Resolver builds a resolver that validates the durable workflow and goober identity.
func (g Generations) Resolver(layout instance.Layout, retainer *configgeneration.Retainer, build GenerationBuilder) GenerationResolver {
	if retainer == nil {
		return nil
	}
	return func(ctx context.Context, identity journal.RunIdentity) (GenerationRuntime, error) {
		// Generated runs require retained source plus parent-generation custody;
		// their display names must never select a mutable catalog definition.
		if identity.Child != nil {
			return GenerationRuntime{}, errors.New("generated child recovery requires retained proposal resolver")
		}
		directory, lease, err := retainer.Store.Acquire(ctx, identity.ConfigGeneration)
		if err != nil {
			return GenerationRuntime{}, err
		}
		defer func() { _ = lease.Release() }()
		set, report, err := g.Load(directory)
		if err != nil {
			return GenerationRuntime{}, fmt.Errorf("load pinned execution definitions: %w (%s)", err, g.IssueSummary(report))
		}
		runtime, err := build(instance.NewLayout(layout.Root).WithConfigDir(directory), identity.ConfigGeneration, set, report, identity)
		if err != nil {
			return GenerationRuntime{}, err
		}
		if runtime.Runner == nil || runtime.Machine == nil {
			return GenerationRuntime{}, errors.New("pinned execution generation does not contain the run workflow")
		}
		if runtime.Machine.Digest() != identity.WorkflowDigest || runtime.GooberDigest != identity.GooberDigest {
			return GenerationRuntime{}, errors.New("pinned execution generation does not match durable workflow/goober identity")
		}
		return runtime, nil
	}
}

// FirstGenerationRetainer returns the optional primary generation owner.
func FirstGenerationRetainer(retainers []*configgeneration.Retainer) *configgeneration.Retainer {
	if len(retainers) == 0 {
		return nil
	}
	return retainers[0]
}

// RetainOptional retains the configuration when an owner is configured.
func (g Generations) RetainOptional(layout instance.Layout, retainers []*configgeneration.Retainer) (instance.Layout, string, error) {
	retainer := FirstGenerationRetainer(retainers)
	if retainer == nil {
		return layout, "", nil
	}
	return g.Retain(context.Background(), layout, retainer)
}

// PinnedConfig loads retained configuration and returns its lease release function.
func (g Generations) PinnedConfig(ctx context.Context, layout instance.Layout, generation string) (*instance.ConfigSet, func(), error) {
	store, err := ExecutionGenerationStore(layout)
	if err != nil {
		return nil, nil, err
	}
	directory, lease, err := store.Acquire(ctx, generation)
	if err != nil {
		return nil, nil, err
	}
	release := func() { _ = lease.Release() }
	set, report, err := g.Load(directory)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("load pinned credential definitions: %w (%s)", err, g.IssueSummary(report))
	}
	return set, release, nil
}

// LastAppliedExecutionGeneration reads the last committed catalog from the instance journal.
//
// The instance journal is the authority for the last committed catalog, even
// when a previously retained generation is reused without producing a new run.
func LastAppliedExecutionGeneration(layout instance.Layout) (string, error) {
	return AppliedExecutionGenerationReader(layout)(context.Background())
}

// AppliedExecutionGenerationReader tracks the latest authoritative generation incrementally.
func AppliedExecutionGenerationReader(layout instance.Layout) func(context.Context) (string, error) {
	latest := journal.NewInstanceLatest(layout.SchedulerDir(), journal.IsConfigGenerationAuthority)
	return func(ctx context.Context) (string, error) {
		event, _, err := latest.Read(ctx)
		generation, _ := event.Runner["configGeneration"].(string)
		return generation, err
	}
}
