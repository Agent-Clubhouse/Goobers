package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workbenchservice"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// prRepairCustodian supports the local single-daemon, per-stage-worktree
// topology. Shared/Temporal/pinned ownership needs a separate qualified fence.
type prRepairCustodian struct {
	layout instance.Layout
	setup  *schedulerSetup
	runID  string
}

func (c prRepairCustodian) scope(ctx context.Context, target providers.RepairPullRequest, use func(context.Context) error) error {
	if use == nil || c.setup == nil || c.setup.RunnerRegistry == nil {
		return errors.New("PR repair host custody unavailable")
	}
	return c.setup.PRRepairCustody.use(ctx, func(snapshot prRepairSnapshot) error {
		return c.withSnapshot(ctx, snapshot, target, use)
	})
}
func (c prRepairCustodian) withSnapshot(ctx context.Context, snapshot prRepairSnapshot, target providers.RepairPullRequest, use func(context.Context) error) error {
	clone, err := snapshot.target(target.Repository)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	remaining := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < remaining {
		remaining = time.Until(deadline)
	}
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	held, err := acquireClaimLock(filepath.Join(c.layout.SchedulerDir(), claimLockFileName), "repair-selected-pr", remaining, time.Now())
	if err != nil {
		return err
	}
	defer func() { _ = held.Release() }()
	release, owners, err := c.setup.RunnerRegistry.acquirePRRepairAdmission(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err = c.claims(ctx, target); err != nil {
		return err
	}
	if err = c.runs(ctx, target.Repository, owners); err != nil {
		return err
	}
	managers, err := c.managers(snapshot)
	if err != nil {
		return err
	}
	var visit func(int) error
	visit = func(index int) error {
		if index == len(managers) {
			return use(ctx)
		}
		return managers[index].WithUnoccupiedBranch(ctx, clone, target.Head, func(context.Context) error { return visit(index + 1) })
	}
	return visit(0)
}
func (s prRepairSnapshot) target(repo providers.RepositoryRef) (string, error) {
	if !s.enabled {
		return "", errors.New("PR repair requires local non-shared runner custody")
	}
	var selected *prRepairRepository
	for _, candidate := range s.repositories {
		ref := candidate.identity
		if !resolutionSameRepository(ref, repo) {
			continue
		}
		if selected != nil || candidate.unsupported {
			return "", errors.New("PR repair repository custody is ambiguous or pinned")
		}
		copy := candidate
		selected = &copy
	}
	if selected == nil {
		return "", errors.New("PR repair exact repository is not configured")
	}
	return childRepoCloneURL(apiv1.RepoRef{Provider: apiv1.Provider(selected.identity.Provider), Owner: selected.identity.Owner, Project: selected.identity.Project, Name: selected.identity.Name})
}
func (c prRepairCustodian) managers(snapshot prRepairSnapshot) ([]*worktree.Manager, error) {
	known := map[string]*worktree.Manager{}
	add := func(manager *worktree.Manager) error {
		if manager == nil {
			return nil
		}
		root, err := filepath.EvalSymlinks(manager.Root)
		if err != nil {
			return err
		}
		if prior := known[root]; prior != nil && prior != manager {
			return errors.New("PR repair duplicate worktree managers")
		}
		known[root] = manager
		return nil
	}
	for _, manager := range snapshot.managers {
		if err := add(manager); err != nil {
			return nil, err
		}
	}
	roots, err := c.layout.WorkcopiesDirs()
	if err != nil {
		return nil, err
	}
	if len(roots) > 64 || len(known) > 64 {
		return nil, errors.New("PR repair worktree inventory exceeds bounds")
	}
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		if known[resolved] == nil {
			return nil, errors.New("PR repair retained worktree root has no live custody manager")
		}
	}
	keys := make([]string, 0, len(known))
	for key := range known {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil, errors.New("PR repair worktree manager unavailable")
	}
	result := make([]*worktree.Manager, 0, len(keys))
	for _, key := range keys {
		result = append(result, known[key])
	}
	return result, nil
}
func (c prRepairCustodian) claims(ctx context.Context, target providers.RepairPullRequest) error {
	path := filepath.Join(c.layout.SchedulerDir(), claimLedgerFileName)
	info, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && info.Size() > 4<<20 {
		return errors.New("PR repair claim inventory exceeds bounds")
	}
	ledger, err := localscheduler.OpenClaimLedger(path)
	if err != nil {
		return err
	}
	entries := ledger.Snapshot()
	if len(entries) > 4096 {
		return errors.New("PR repair claim inventory exceeds bounds")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.ItemID != "pr/"+target.ID && entry.ExternalID != "pr/"+target.ID {
			continue
		}
		// The PR namespace has no repository identity in the key. Ambiguous,
		// expired and legacy records are not silently reclaimed by an editor.
		return errors.New("PR repair selected PR locator still has claim custody; repository identity is ambiguous")
	}
	return nil
}
func repairRepoMatches(ref apiv1.RepoRef, target providers.RepositoryRef) bool {
	return resolutionSameRepository(providers.RepositoryRef{Provider: providers.ProviderKind(ref.Provider), Owner: ref.Owner, Project: ref.Project, Name: ref.Name}, target)
}

var _ workbenchservice.PRRepairCustody = prRepairCustodian{}.scope
