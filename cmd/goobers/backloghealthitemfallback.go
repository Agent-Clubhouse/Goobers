package main

import (
	"context"
	"io"
	"strings"

	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// resolveUnledgeredReadyItems is the last resort after a FULL transition scan
// still cannot explain the live ready pool (#6986). The repository-wide walk is
// page-capped and the provider may stop serving very old history, so a ready
// item whose label-add event predates the walk's oldest page has no entry in
// the ledger at all. Failing the stage for that would fail the whole lane every
// time the cursor is invalidated; instead each unexplained item is resolved
// from its own complete label history — one bounded call per missing item —
// and only an item that history cannot explain either is still an error.
//
// The per-item transitions are folded back into the durable ledger so the next
// resumed cycle explains the item without forcing another full rescan.
func resolveUnledgeredReadyItems(
	ctx context.Context,
	issueProvider backlogHealthProvider,
	root string,
	repo providers.RepositoryRef,
	items []providers.WorkItem,
	readyLabel string,
	filtered []providers.WorkItemLabelTransition,
	scan backlogHealthScan,
	stdout io.Writer,
) ([]providers.WorkItemLabelTransition, backlogHealthScan, error) {
	missing := unexplainedReadyItems(items, readyLabel, filtered)
	if repo.Provider == providers.ProviderADO || len(missing) == 0 {
		err := annotateBacklogReadyTimes(repo.Provider, items, readyLabel, filtered)
		return nil, scan, &backlogReadyLedgerError{what: "snapshot ready backlog", err: err}
	}
	ids := make([]string, 0, len(missing))
	for _, item := range missing {
		ids = append(ids, item.ID)
	}
	pf(stdout, "full ready-transition scan (reason=%s, pages=%d) does not explain ready item(s) %s; "+
		"resolving them from per-item label history\n", scan.Reason, scan.Pages, strings.Join(ids, ","))

	var fetched []providers.WorkItemLabelTransition
	for _, item := range missing {
		itemTransitions, err := backlogHealthItemTransitions(ctx, issueProvider, repo, item, readyLabel)
		if err != nil {
			return nil, scan, classifyBacklogReadyLedgerError(err)
		}
		fetched = append(fetched, itemTransitions...)
	}
	resolved := mergeLabelTransitions(filtered, fetched)
	if err := annotateBacklogReadyTimes(repo.Provider, items, readyLabel, resolved); err != nil {
		return nil, scan, &backlogReadyLedgerError{what: "snapshot ready backlog", err: err}
	}
	scan.ItemLookups = len(missing)

	if _, ok := issueProvider.(labelTransitionScanner); ok && scan.ToEventID > 0 {
		size, err := foldItemTransitionsIntoLedger(ctx, root, repo, readyLabel, fetched)
		if err != nil {
			// The ready times above are already authoritative; only the cache of
			// them failed to persist, which costs the next cycle a rescan.
			pf(stdout, "warning: could not record per-item ready transitions in the ledger: %v\n", err)
		} else if size > 0 {
			scan.LedgerSize = size
		}
	}
	return resolved, scan, nil
}

// unexplainedReadyItems returns the open ready items with no active label-add
// event among transitions.
func unexplainedReadyItems(
	items []providers.WorkItem,
	readyLabel string,
	transitions []providers.WorkItemLabelTransition,
) []providers.WorkItem {
	active := activeReadyTimes(readyLabel, transitions)
	var missing []providers.WorkItem
	for _, item := range items {
		if !needsReadyTime(item, readyLabel) {
			continue
		}
		if _, ok := active[item.ID]; !ok {
			missing = append(missing, item)
		}
	}
	return missing
}

// foldItemTransitionsIntoLedger merges per-item transitions into the existing
// durable ledger and returns its new size, or zero when nothing was written.
// It never creates or repairs a ledger: with no decodable cursor there is no
// high-water mark to keep the ledger self-consistent against, so the next
// cycle's own full scan owns that. Transitions newer than the persisted
// high-water mark are dropped for the same reason — the cursor decoder rejects
// a ledger that contains them — and the next resumed scan collects them anyway.
func foldItemTransitionsIntoLedger(
	ctx context.Context,
	root string,
	repo providers.RepositoryRef,
	label string,
	fresh []providers.WorkItemLabelTransition,
) (int, error) {
	if len(fresh) == 0 {
		return 0, nil
	}
	store, err := openStageStateStore(layoutFor(root))
	if err != nil {
		return 0, err
	}
	gaggle := providerGaggle()
	size := 0
	err = updateJSONState(
		ctx, store, backlogHealthCursorKey(gaggle, repo, label), stateLockOperationBacklogHealthCursor,
		func(value stateclient.Value) (backlogHealthCursor, error) {
			cursor, reason := decodeBacklogHealthCursor(value, gaggle, repo, label)
			if reason != "" {
				return backlogHealthCursor{}, nil
			}
			return cursor, nil
		},
		encodeBacklogHealthCursor,
		func(current backlogHealthCursor) (backlogHealthCursor, bool, error) {
			size = 0
			if current.HighWaterEventID <= 0 {
				return current, false, nil
			}
			var covered []providers.WorkItemLabelTransition
			for _, transition := range fresh {
				if transition.EventID > 0 && transition.EventID <= current.HighWaterEventID &&
					transition.ItemID != "" && transition.Label == label && !transition.OccurredAt.IsZero() {
					covered = append(covered, transition)
				}
			}
			if len(covered) == 0 {
				return current, false, nil
			}
			current.Transitions = mergeLabelTransitions(current.Transitions, covered)
			size = len(current.Transitions)
			return current, true, nil
		},
	)
	if err != nil {
		return 0, err
	}
	return size, nil
}
