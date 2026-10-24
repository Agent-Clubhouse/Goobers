// Package terminalwriter is the cloud control role's providerwriter.Writer:
// the one path through which terminal hooks write to a repository provider.
//
// A call names a run, never a credential. The writer binds the run to the
// repositories its journal identity and pinned config generation declare, and
// refuses any other target (providerwriter.ErrRepositoryNotPinned). Credentials
// come from the applied instance configuration, scoped to the bound repository
// and to the one capability the operation needs.
package terminalwriter

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

// Binding is the provider surface one run may write to.
type Binding struct {
	RunID      string
	Gaggle     string
	Generation string
	// Project is the gaggle's pinned project repository as configured, the key
	// its credentials are bound under.
	Project apiv1.RepoRef
	// Workspace is the repository the run's branches and pull requests live in.
	Workspace providers.RepositoryRef
	// WorkItems is the repository (or ADO project) its work items live in.
	WorkItems providers.RepositoryRef
}

// Runs resolves a run's Binding.
type Runs interface {
	Bind(ctx context.Context, runID string) (Binding, error)
}

// RunOpener opens a run's journal; journalpg.Backend satisfies it.
type RunOpener interface {
	OpenRun(ctx context.Context, ref journal.RunRef) (journal.RunReader, error)
}

// JournalRuns binds a run from its cloud journal identity and the config
// generation it pinned. Nothing the caller supplies is trusted.
type JournalRuns struct {
	Journal     RunOpener
	Generations childworkflow.GenerationSource
}

var _ Runs = JournalRuns{}

// Bind reads runID's identity and pinned generation. A run with no pinned
// gaggle or generation, or a gaggle the generation does not declare, can never
// be written for: it fails with providerwriter.ErrRepositoryNotPinned.
func (r JournalRuns) Bind(ctx context.Context, runID string) (Binding, error) {
	if r.Journal == nil || r.Generations == nil {
		return Binding{}, errors.New("terminal writer requires a journal and a generation source")
	}
	reader, err := r.Journal.OpenRun(ctx, journal.RunRef{ID: runID, ReadOnly: true})
	if err != nil {
		return Binding{}, fmt.Errorf("open run %s: %w", runID, err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return Binding{}, fmt.Errorf("read run %s identity: %w", runID, err)
	}
	if identity.Gaggle == "" || identity.ConfigGeneration == "" {
		return Binding{}, fmt.Errorf("run %s pinned no gaggle generation: %w", runID, providerwriter.ErrRepositoryNotPinned)
	}
	generation, err := r.Generations.Acquire(ctx, identity.ConfigGeneration)
	if err != nil {
		return Binding{}, fmt.Errorf("pin run %s generation: %w", runID, err)
	}
	// Only the declared repositories are copied out; an unpin failure cannot
	// change them.
	defer func() { _ = generation.Release() }()
	project := runtimewiring.GaggleProjectRef(generation.Set, identity.Gaggle)
	if project.Owner == "" || project.Name == "" {
		return Binding{}, fmt.Errorf("run %s gaggle %q has no pinned project: %w", runID, identity.Gaggle, providerwriter.ErrRepositoryNotPinned)
	}
	// A run that worked in another repository journals it; engine runs use the
	// gaggle's project (childworkflow.ExecutionBindingFor applies the same rule).
	workspace := project
	if identity.WorkspaceRepository != nil {
		workspace = *identity.WorkspaceRepository
	}
	workspaceRef := repositoryRef(workspace)
	workItems, err := providerconfig.ApplyBacklogProject(generation.Set, identity.Gaggle, repositoryRef(project))
	if err != nil {
		return Binding{}, fmt.Errorf("run %s backlog: %w", runID, errors.Join(err, providerwriter.ErrRepositoryNotPinned))
	}
	return Binding{
		RunID:      runID,
		Gaggle:     identity.Gaggle,
		Generation: identity.ConfigGeneration,
		Project:    project,
		Workspace:  workspaceRef,
		WorkItems:  workItems,
	}, nil
}

func repositoryRef(repo apiv1.RepoRef) providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
}

// sameRepository compares two refs by provider, project, owner and name. The
// service URL and provider ID are dropped: a configured backlog ref carries a
// base URL a hook's ref need not restate.
func sameRepository(a, b providers.RepositoryRef) bool {
	a.URL, a.ID, b.URL, b.ID = "", "", "", ""
	return a.CanonicalKey() == b.CanonicalKey()
}
