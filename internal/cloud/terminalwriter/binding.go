// Package terminalwriter is the cloud control role's providerwriter.Writer:
// the one path through which terminal hooks write to a repository provider.
//
// A call names a run, never a credential. The writer binds the run to the
// repositories its start-fence pins and pinned config generation declare, and
// refuses any other target (providerwriter.ErrRepositoryNotPinned). Credentials
// come from the applied instance configuration, scoped to the bound repository
// and to the one capability the operation needs.
package terminalwriter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/internal/triggerqueue"
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

// RunPin is what the start fence recorded for a run in control.run_pin: the
// fields of acceptance.RunPins the writer binds from.
type RunPin struct {
	Gaggle           string
	ConfigGeneration string
	RepoRef          apiv1.RepoRef
}

// PinSource reads the start fence's pin for a run. A run with no pin returns
// an error matching sql.ErrNoRows, as acceptance.Store.RunPinsOf does.
type PinSource func(ctx context.Context, runID string) (RunPin, error)

// ChildLink is the durable control-held parent link for a generated child.
type ChildLink struct {
	Gaggle          string
	ParentRunID     string
	StageOccurrence string
	InvocationKey   string
	AcceptanceID    string
	Tombstoned      bool
}

// ChildSource reads generated-child custody by the immutable execution run ID.
// A run with no child custody returns an error matching sql.ErrNoRows.
type ChildSource func(ctx context.Context, runID string) (ChildLink, error)

// RunOpener opens a run's journal; journalpg.Backend satisfies it.
type RunOpener interface {
	OpenRun(ctx context.Context, ref journal.RunRef) (journal.RunReader, error)
}

// JournalRuns binds a run from control-held state only, as the engine child
// authority does (childengine.ParentAuthority): the gaggle, generation and
// repository come from the start fence's pins. The journal is written by the
// worker, so it is only an agreement check; a journal that disagrees refuses.
type JournalRuns struct {
	Pins        PinSource
	Children    ChildSource
	Journal     RunOpener
	Generations childworkflow.GenerationSource
}

var _ Runs = JournalRuns{}

// Bind binds runID. Anything that can never bind (no own or inherited pin, an
// incomplete pin, no journal, a journal that disagrees with control custody, a
// repository the pinned generation does not declare) fails with
// providerwriter.ErrRepositoryNotPinned, which is permanent; a failed read is
// returned as is, to be retried.
func (r JournalRuns) Bind(ctx context.Context, runID string) (Binding, error) {
	if r.Pins == nil || r.Journal == nil || r.Generations == nil {
		return Binding{}, errors.New("terminal writer requires run pins, a journal and a generation source")
	}
	if !apiv1.ValidRunID(runID) {
		return Binding{}, notPinned(runID, "is not a valid run ID")
	}
	pin, child, err := r.bindingPin(ctx, runID)
	if err != nil {
		return Binding{}, err
	}
	if missing := missingPin(pin); missing != "" {
		return Binding{}, notPinned(runID, "has no pinned "+missing)
	}
	if err := r.journalAgrees(ctx, runID, pin, child); err != nil {
		return Binding{}, err
	}
	generation, err := r.Generations.Acquire(ctx, pin.ConfigGeneration)
	if err != nil {
		return Binding{}, fmt.Errorf("pin run %s generation: %w", runID, err)
	}
	// Only the declared repositories are copied out; an unpin failure cannot
	// change them.
	defer func() { _ = generation.Release() }()
	project := runtimewiring.GaggleProjectRef(generation.Set, pin.Gaggle)
	if project.Owner == "" || project.Name == "" {
		return Binding{}, notPinned(runID, fmt.Sprintf("gaggle %q has no project in generation %s", pin.Gaggle, pin.ConfigGeneration))
	}
	workspace := repositoryRef(pin.RepoRef)
	if !sameRepository(workspace, repositoryRef(project)) {
		return Binding{}, notPinned(runID, fmt.Sprintf("pinned repository %s is not gaggle %q's project", workspace.CanonicalKey(), pin.Gaggle))
	}
	workItems, err := providerconfig.ApplyBacklogProject(generation.Set, pin.Gaggle, repositoryRef(project))
	if err != nil {
		return Binding{}, fmt.Errorf("run %s backlog: %w", runID, errors.Join(err, providerwriter.ErrRepositoryNotPinned))
	}
	return Binding{
		RunID:      runID,
		Gaggle:     pin.Gaggle,
		Generation: pin.ConfigGeneration,
		Project:    project,
		Workspace:  workspace,
		WorkItems:  workItems,
	}, nil
}

// bindingPin uses a run's own start-fence pin, or for a durably admitted child,
// its parent's pin. The child lookup is control-held custody, never caller input.
func (r JournalRuns) bindingPin(ctx context.Context, runID string) (RunPin, *ChildLink, error) {
	pin, err := r.Pins(ctx, runID)
	if err == nil {
		return pin, nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RunPin{}, nil, fmt.Errorf("read run %s pin: %w", runID, err)
	}
	if r.Children == nil {
		return RunPin{}, nil, notPinned(runID, "has no start-fence pin")
	}
	child, err := r.Children(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return RunPin{}, nil, notPinned(runID, "has no start-fence pin and no durable child custody")
	}
	if err != nil {
		return RunPin{}, nil, fmt.Errorf("read run %s child custody: %w", runID, err)
	}
	if child.Tombstoned || child.Gaggle == "" || !apiv1.ValidRunID(child.ParentRunID) ||
		child.ParentRunID == runID || child.StageOccurrence == "" ||
		child.InvocationKey == "" || child.AcceptanceID != "trigger-"+runID {
		return RunPin{}, nil, notPinned(runID, "has invalid durable child custody")
	}
	pin, err = r.Pins(ctx, child.ParentRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return RunPin{}, nil, notPinned(runID, fmt.Sprintf("parent run %s has no start-fence pin", child.ParentRunID))
	}
	if err != nil {
		return RunPin{}, nil, fmt.Errorf("read run %s parent %s pin: %w", runID, child.ParentRunID, err)
	}
	if pin.Gaggle != child.Gaggle {
		return RunPin{}, nil, notPinned(runID, "durable child custody disagrees with its parent's pinned gaggle")
	}
	return pin, &child, nil
}

// journalAgrees requires the worker-written identity to agree with the pin:
// same run, gaggle and generation, and a journaled workspace, if any, equal to
// the pinned repository. A child must also carry the exact control-held lineage
// link through which it inherited the parent's pin. It is deliberately a
// subset of childengine's journalAgrees: the workflow, digests and driver gate
// child-workflow authority, not which repository a terminal hook may write.
func (r JournalRuns) journalAgrees(ctx context.Context, runID string, pin RunPin, child *ChildLink) error {
	reader, err := r.Journal.OpenRun(ctx, journal.RunRef{ID: runID, ReadOnly: true})
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, journal.ErrNotRunDirectory) {
		return notPinned(runID, "has no journal")
	}
	if err != nil {
		return fmt.Errorf("open run %s: %w", runID, err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return fmt.Errorf("read run %s identity: %w", runID, err)
	}
	agrees := identity.RunID == runID && identity.Gaggle == pin.Gaggle && identity.ConfigGeneration == pin.ConfigGeneration &&
		(identity.WorkspaceRepository == nil || reflect.DeepEqual(*identity.WorkspaceRepository, pin.RepoRef))
	if !agrees {
		return notPinned(runID, "journal identity disagrees with its binding pin")
	}
	if child != nil {
		lineage := identity.Child
		agrees = lineage != nil && lineage.Gaggle == child.Gaggle &&
			lineage.ParentRunID == child.ParentRunID &&
			lineage.StageOccurrence == child.StageOccurrence &&
			lineage.InvocationKey == child.InvocationKey &&
			lineage.AcceptanceID == child.AcceptanceID
		if !agrees {
			return notPinned(runID, "journal child lineage disagrees with durable child custody")
		}
	}
	return nil
}

// missingPin names the first pin the bind needs that the start left empty.
func missingPin(pin RunPin) string {
	switch {
	case pin.Gaggle == "":
		return "gaggle"
	case pin.ConfigGeneration == "":
		return "config generation"
	case pin.RepoRef.Owner == "" || pin.RepoRef.Name == "":
		return "repository"
	}
	return ""
}

func notPinned(runID, why string) error {
	return fmt.Errorf("run %s %s: %w", runID, why, providerwriter.ErrRepositoryNotPinned)
}

func repositoryRef(repo apiv1.RepoRef) providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), Owner: repo.Owner, Project: repo.Project, Name: repo.Name, URL: repo.BaseURL}
}

// sameRepository compares two refs by provider, project, owner and name. The
// service URL and provider ID are dropped: a configured backlog ref carries a
// base URL a hook's ref need not restate.
func sameRepository(a, b providers.RepositoryRef) bool {
	a.URL, a.ID, b.URL, b.ID = "", "", "", ""
	return a.CanonicalKey() == b.CanonicalKey()
}

// AcceptancePins adapts the start fence's pin reader
// (acceptance.Store.RunPinsOf) to a PinSource. Its sql.ErrNoRows for a run
// with no pin passes through, so Bind refuses it permanently.
func AcceptancePins(pinsOf func(ctx context.Context, runID string) (acceptance.RunPins, error)) PinSource {
	return func(ctx context.Context, runID string) (RunPin, error) {
		pins, err := pinsOf(ctx, runID)
		if err != nil {
			return RunPin{}, err
		}
		return RunPin{Gaggle: pins.Gaggle, ConfigGeneration: pins.ConfigGeneration, RepoRef: pins.RepoRef}, nil
	}
}

// AcceptanceChildren adapts the acceptance store's durable child-custody read
// (acceptance.Store.ChildForExecutionRun) to a ChildSource.
func AcceptanceChildren(childForExecutionRun func(ctx context.Context, runID string) (triggerqueue.ChildRecord, error)) ChildSource {
	return func(ctx context.Context, runID string) (ChildLink, error) {
		child, err := childForExecutionRun(ctx, runID)
		if err != nil {
			return ChildLink{}, err
		}
		return ChildLink{
			Gaggle:          child.Identity.Gaggle,
			ParentRunID:     child.Identity.ParentRunID,
			StageOccurrence: child.Identity.StageOccurrence,
			InvocationKey:   child.Identity.InvocationKey,
			AcceptanceID:    child.AcceptanceID,
			Tombstoned:      !child.TombstonedAt.IsZero(),
		}, nil
	}
}
