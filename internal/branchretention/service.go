package branchretention

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/instanceannotations"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// Service composes conservative retention with instance-owned authority lookups.
type Service struct {
	Root                  string
	Repositories          func(string) (map[string]instanceannotations.ItemRepository, error)
	ItemParked            func(context.Context, string, string, instanceannotations.ItemRepository) (bool, error)
	KindAuthorizesCustody func(string) bool
	UnknownRepository     error
}

func readRun(runsDir, runID string) (journal.RunIdentity, []journal.Event, error) {
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

// Allowed requires settled history and authoritative, currently unparked items.
// Retained branches can outlive active claims: consult selection-time repository
// records, including released claims; never guess legacy ownership.
func (s Service) Allowed(ctx context.Context, runsDir, runID string) (bool, error) {
	identity, events, err := readRun(runsDir, runID)
	if err != nil || !Settled(events) {
		return false, err
	}
	recorded, err := s.Repositories(runID)
	if err != nil {
		return false, err
	}
	if len(recorded) == 0 {
		return false, fmt.Errorf("%w: retention run %s", s.UnknownRepository, runID)
	}
	for _, id := range ClaimedItemIDs(identity, events) {
		if _, ok := recorded[id]; !ok {
			return false, fmt.Errorf("%w: retention run %s item %s", s.UnknownRepository, runID, id)
		}
	}
	ids := make([]string, 0, len(recorded))
	for id := range recorded {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := recorded[id]
		if entry.Repository.Provider == "" || entry.Repository.Owner == "" || entry.Repository.Name == "" || !s.KindAuthorizesCustody(entry.Kind) {
			return false, s.UnknownRepository
		}
		parked, err := s.ItemParked(ctx, s.Root, id, entry)
		if err != nil {
			return false, fmt.Errorf("retention item %s: %w", id, err)
		}
		if parked {
			return false, nil
		}
	}
	return true, nil
}

// Configure supplies the existing discovery and pre-delete retention checks.
func (s Service) Configure(ctx context.Context, runsByRoot map[string]string, branchReferences map[string]map[string][]string, opts *worktree.RetentionOptions) {
	opts.RunTerminalAt = func(root, runID string) (time.Time, error) {
		identity, events, err := readRun(runsByRoot[root], runID)
		if err != nil {
			return time.Time{}, err
		}
		return TerminalAt(identity, events, opts.Now)
	}
	opts.CanPruneBranch = func(root, runID, branch string) (bool, error) {
		seen := make(map[string]bool)
		for _, owner := range append([]string{runID}, branchReferences[root][branch]...) {
			if seen[owner] {
				continue
			}
			seen[owner] = true
			allowed, err := s.Allowed(ctx, runsByRoot[root], owner)
			if err != nil || !allowed {
				return false, err
			}
		}
		return true, nil
	}
	opts.IsRunTerminal = func(root, runID string) (bool, error) {
		_, events, err := readRun(runsByRoot[root], runID)
		return err == nil && Settled(events), err
	}
	opts.IsBranchProtected = func(root, branch string) (bool, error) {
		for _, runID := range branchReferences[root][branch] {
			_, events, err := readRun(runsByRoot[root], runID)
			if err != nil || !Settled(events) {
				return true, err
			}
		}
		return false, nil
	}
}
