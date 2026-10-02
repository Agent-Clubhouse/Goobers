package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/branchretention"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func retentionRun(runsDir, runID string) (journal.RunIdentity, []journal.Event, error) {
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		return journal.RunIdentity{}, nil, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return identity, nil, err
	}
	events, err := reader.Events()
	return identity, events, err
}

// Retained branches can outlive their active claims. Consult selection-time
// repository records, including released claims; never guess legacy ownership.
func retentionBranchAllowed(ctx context.Context, l instance.Layout, runsDir, runID string) (bool, error) {
	identity, events, err := retentionRun(runsDir, runID)
	if err != nil || !branchretention.Settled(events) {
		return false, err
	}
	recorded, err := annotationsForInstance(l.SchedulerDir()).allItemRepositories(l.SchedulerDir(), runID)
	if err != nil {
		return false, err
	}
	if len(recorded) == 0 {
		return false, fmt.Errorf("%w: retention run %s", ErrItemRepositoryUnknown, runID)
	}
	for _, id := range branchretention.ClaimedItemIDs(identity, events) {
		if _, ok := recorded[id]; !ok {
			return false, fmt.Errorf("%w: retention run %s item %s", ErrItemRepositoryUnknown, runID, id)
		}
	}
	ids := make([]string, 0, len(recorded))
	for id := range recorded {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := recorded[id]
		if entry.repo.Provider == "" || entry.repo.Owner == "" || entry.repo.Name == "" || !recoveryItemKindAuthorizesCustody(entry.kind) {
			return false, ErrItemRepositoryUnknown
		}
		parked, err := retentionItemParked(ctx, l.Root, id, entry)
		if err != nil {
			return false, fmt.Errorf("retention item %s: %w", id, err)
		}
		if parked {
			return false, nil
		}
	}
	return true, nil
}

// No read cache: both discovery and pre-delete checks need current provider
// state. These are bounded, read-only calls and never release or mutate claims.
var retentionItemParked = func(ctx context.Context, root, id string, entry recordedItemRepo) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	provider, err := newProviderForStage(root, entry.repo, true, withStageProviderConfiguredADOAuth())
	if err != nil {
		return false, err
	}
	if entry.kind == itemKindPullRequest {
		item, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: entry.repo, PullID: id})
		if err == nil && item.State == "" {
			return false, fmt.Errorf("pull request has no current state")
		}
		return branchretention.Parked(item.State, item.Labels), err
	}
	item, err := provider.GetWorkItem(ctx, entry.repo, id)
	if err == nil && item.State == "" && item.Status == "" {
		return false, fmt.Errorf("item has no current state")
	}
	return branchretention.Parked(item.State, item.Labels) || branchretention.Parked(string(item.Status), nil) || item.BlockedByCount > 0, err
}

func configureBranchRetention(ctx context.Context, l instance.Layout, runsByRoot map[string]string, branchReferences map[string]map[string][]string, opts *worktree.RetentionOptions) {
	opts.RunTerminalAt = func(root, runID string) (time.Time, error) {
		identity, events, err := retentionRun(runsByRoot[root], runID)
		if err != nil {
			return time.Time{}, err
		}
		return branchretention.TerminalAt(identity, events, opts.Now)
	}
	opts.CanPruneBranch = func(root, runID, branch string) (bool, error) {
		seen := make(map[string]bool)
		for _, owner := range append([]string{runID}, branchReferences[root][branch]...) {
			if seen[owner] {
				continue
			}
			seen[owner] = true
			allowed, err := retentionBranchAllowed(ctx, l, runsByRoot[root], owner)
			if err != nil || !allowed {
				return false, err
			}
		}
		return true, nil
	}
	opts.IsRunTerminal = func(root, runID string) (bool, error) {
		_, events, err := retentionRun(runsByRoot[root], runID)
		return err == nil && branchretention.Settled(events), err
	}
	opts.IsBranchProtected = func(root, branch string) (bool, error) {
		for _, runID := range branchReferences[root][branch] {
			_, events, err := retentionRun(runsByRoot[root], runID)
			if err != nil || !branchretention.Settled(events) {
				return true, err
			}
		}
		return false, nil
	}
}
