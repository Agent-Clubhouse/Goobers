package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/nowork"
	"github.com/goobers/goobers/internal/nowork/settle"
	"github.com/goobers/goobers/providers"
)

// noWorkTerminalForRun reads the completed run's journal for its terminal verdict.
func noWorkTerminalForRun(l instance.Layout, runID, finalState string) (nowork.Terminal, bool, error) {
	if finalState == "" {
		return nowork.Terminal{}, false, nil
	}
	reader, err := journal.OpenReadOnly(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return nowork.Terminal{}, false, fmt.Errorf("open journal for run %q: %w", runID, err)
	}
	events, err := reader.Events()
	if err != nil {
		return nowork.Terminal{}, false, fmt.Errorf("read journal for run %q: %w", runID, err)
	}
	terminal, ok := nowork.TerminalFromEvents(events, finalState)
	return terminal, ok, nil
}

// settleNoWorkStreak is the completed-terminal entry point; the policy is
// settle.Settle, shared with the cloud controller.
func settleNoWorkStreak(
	ctx context.Context,
	poster gate.Commenter,
	l instance.Layout,
	runID, finalState, runURL string,
) error {
	return settle.Settle(ctx, poster, settle.Deps{
		Items: func(runID string) ([]settle.Item, error) {
			claimed, err := claimedItemsForRun(l, runID)
			items := make([]settle.Item, 0, len(claimed))
			for _, c := range claimed {
				items = append(items, settle.Item{ItemID: c.ItemID, Repo: c.Repo, Curation: c.Purpose == itemPurposeCuration})
			}
			return items, err
		},
		Terminal: func(runID, finalState string) (nowork.Terminal, bool, error) {
			return noWorkTerminalForRun(l, runID, finalState)
		},
		Increment: func(ctx context.Context, repo providers.RepositoryRef, itemID, runID string, t nowork.Terminal) (nowork.Record, nowork.Record, error) {
			return incrementNoWorkStreak(ctx, l, repo, itemID, runID, t)
		},
		Reset: func(ctx context.Context, repo providers.RepositoryRef, itemID, runID string) (nowork.Record, error) {
			return resetNoWorkStreakState(ctx, l, repo, itemID, runID)
		},
	}, runID, finalState, runURL)
}
