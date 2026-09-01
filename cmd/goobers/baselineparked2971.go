package main

import (
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/baseline"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// Parked-subject selection guard (#2971).
//
// A subject parked on a shared baseline failure is waiting on a repair to the
// target branch, not on anything its own diff can do. Claiming it again spends
// a full agentic attempt to re-derive that, so backlog selection skips it for
// as long as the park is current — the same shape as the learned dependency
// block (#552), but keyed on a baseline blocker instead of an issue.
//
// The park is NOT permanent and nothing here makes it so: the runner releases
// waiters as soon as the base advances past the failure or the baseline goes
// green (runner.releaseBaselineParks), and this guard then stops skipping them
// on the very next tick. This is a local-state read: an instance that never
// enabled base-health detection has no store and is filtered exactly as before.
func filterBaselineParkedItems(
	l instance.Layout,
	repo providers.RepositoryRef,
	eligible []providers.WorkItem,
) (filtered []providers.WorkItem, skipped []string, warnings []string) {
	if len(eligible) == 0 {
		return eligible, nil, nil
	}
	store, err := baseline.OpenStore(filepath.Join(l.Root, baselineStateFileName))
	if err != nil {
		// Fail open: an unreadable baseline cache must never hide work. The
		// worst case is one run that re-derives a known shared failure, which
		// is the pre-#2971 behaviour, not a regression.
		return eligible, nil, []string{fmt.Sprintf("read baseline blockers: %v", err)}
	}
	parked := map[string]string{}
	for _, blocker := range store.Blockers(baseline.RepoKey(repo.Owner, repo.Name)) {
		if blocker.Resolved {
			continue
		}
		for _, waiter := range blocker.Waiting {
			if waiter.Subject != "" {
				parked[waiter.Subject] = blocker.Key
			}
		}
	}
	if len(parked) == 0 {
		return eligible, nil, nil
	}
	filtered = make([]providers.WorkItem, 0, len(eligible))
	for _, item := range eligible {
		if key, ok := parked[item.ID]; ok {
			skipped = append(skipped, fmt.Sprintf("%s waits on shared baseline failure %s", item.ID, key))
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered, skipped, nil
}
