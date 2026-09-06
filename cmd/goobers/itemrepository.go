package main

import (
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// itemrepository.go durably records the typed repository identity (and
// issue/PR kind) a work item was actually SELECTED against, at the moment it
// is claimed (#4417). Terminal notifications and the circuit breaker
// previously reconstructed "which repo does this run's item belong to" from
// the gaggle's statically configured project — correct for a single-repo
// gaggle, but wrong whenever a mid-run-claimed item (backlog-query,
// pr-remediation's own PR claim, decomposition source selection) belongs to
// a different repo than the gaggle's declared project on a multi-repo
// instance (GaggleSpec.AdditionalRepos), or when wiring itself drifts from
// the run it is actually processing. #4417's own incident was exactly this:
// a terminal circuit-breaker path routed to a completely unrelated
// repository because it never had the item's own identity to consult.
//
// The mechanism mirrors failureStreakAnnotation (#4364): a
// journal.EventRunnerAnnotation written via the stage-annotation seam
// (stageannotations.go), which already works identically from a self-runner
// or a mode-3 stage pod, and read back by scanning the instance log — the
// same durable, cross-run bookkeeping every other terminal decision in this
// package already relies on, rather than a live provider call or a new
// claims-plane wire field.

// itemRepoAnnotation marks a runner.annotation event as one claimed item's
// repository-identity record.
const itemRepoAnnotation = "item-repo"

// Item kinds recorded alongside a repository identity (AC: provider, owner,
// repository, kind, and number together).
const (
	itemKindIssue       = "issue"
	itemKindPullRequest = "pull_request"
)

// itemRepoKey identifies one claimed item's repo-identity annotation, scoped
// by run: two different runs claiming the same item ID (never concurrently,
// under the ledger's own exclusivity, but sequentially across attempts) each
// record their own selection-time snapshot.
func itemRepoKey(runID, itemID string) string {
	return runID + "#" + itemID
}

// ErrItemRepositoryUnknown is the fail-closed contract error terminal
// bookkeeping returns when a claimed item has no recorded repository
// identity. Never a fallback to the gaggle's configured project or
// cfg.Repos[0] — reconstructing ownership from either is precisely the
// defect #4417 exists to eliminate, so a missing identity must surface as an
// explicit, actionable error before any provider call rather than silently
// mis-route.
var ErrItemRepositoryUnknown = errors.New("goobers: no recorded repository identity for claimed item")

// recordItemRepository durably annotates the typed repository and kind
// itemID was selected against, for runID, via the stage-annotation seam so
// it works identically whether the caller is the daemon's own stage
// subprocess or a mode-3 pod. Call immediately after a successful claim, at
// every selection site — the repository must already be fully qualified
// (provider, owner, name) at that point, since selection is exactly where a
// real one was resolved to make the provider call that found the item.
func recordItemRepository(annotations stageAnnotator, runID, itemID, kind string, repo providers.RepositoryRef) error {
	if repo.Provider == "" || repo.Owner == "" || repo.Name == "" {
		return fmt.Errorf(
			"record repository identity for %s: incomplete repository (provider=%q owner=%q name=%q)",
			itemID, repo.Provider, repo.Owner, repo.Name,
		)
	}
	return annotations.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: runID,
		Runner: map[string]any{
			"annotation": itemRepoAnnotation,
			"key":        itemRepoKey(runID, itemID),
			"itemId":     itemID,
			"kind":       kind,
			"provider":   string(repo.Provider),
			"owner":      repo.Owner,
			"project":    repo.Project,
			"name":       repo.Name,
		},
	})
}

// claimedItem pairs one claimed item ID with the typed repository and kind
// recorded for it at selection time.
type claimedItem struct {
	ItemID string
	Kind   string
	Repo   providers.RepositoryRef
}

// claimedItemsForRun resolves every item runID's claim ledger entries name,
// paired with the repository identity recordItemRepository recorded for
// each at selection time. This is the circuit breaker's and terminal
// notifier's own lookup (applyCircuitBreaker, resetCircuitBreaker) — a
// separate, richer sibling of claimedItemIDsForRun (whose plain []string
// contract other callers, e.g. runner.Config.ClaimedItems and
// implementcontext.go, still want unchanged). Fails closed with
// ErrItemRepositoryUnknown the moment any claimed item has no recorded
// identity, before returning — a caller must never partially trust the
// result and mix a real repo for one item with a guess for another.
func claimedItemsForRun(l instance.Layout, runID string) ([]claimedItem, error) {
	ids, err := claimedItemIDsForRun(l, runID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	recorded, err := loadItemRepositories(l, runID, ids)
	if err != nil {
		return nil, err
	}
	items := make([]claimedItem, 0, len(ids))
	for _, id := range ids {
		entry, ok := recorded[id]
		if !ok {
			return nil, fmt.Errorf("%w: run %s item %s", ErrItemRepositoryUnknown, runID, id)
		}
		items = append(items, claimedItem{ItemID: id, Kind: entry.kind, Repo: entry.repo})
	}
	return items, nil
}

type recordedItemRepo struct {
	repo providers.RepositoryRef
	kind string
}

// loadItemRepositories scans the instance log for item-repo annotations
// matching runID and any of itemIDs — the same newest-wins scan
// loadFailureStreakCount uses, so a re-claim across attempts of the same run
// (which re-records the identity) always resolves to the latest write.
func loadItemRepositories(l instance.Layout, runID string, itemIDs []string) (map[string]recordedItemRepo, error) {
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		return nil, fmt.Errorf("read instance log for item repositories: %w", err)
	}
	want := make(map[string]struct{}, len(itemIDs))
	for _, id := range itemIDs {
		want[itemRepoKey(runID, id)] = struct{}{}
	}
	found := make(map[string]recordedItemRepo, len(itemIDs))
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["annotation"] != itemRepoAnnotation {
			continue
		}
		key, _ := event.Runner["key"].(string)
		if _, ok := want[key]; !ok {
			continue
		}
		itemID, _ := event.Runner["itemId"].(string)
		provider, _ := event.Runner["provider"].(string)
		owner, _ := event.Runner["owner"].(string)
		project, _ := event.Runner["project"].(string)
		name, _ := event.Runner["name"].(string)
		kind, _ := event.Runner["kind"].(string)
		found[itemID] = recordedItemRepo{
			repo: providers.RepositoryRef{
				Provider: providers.ProviderKind(provider),
				Owner:    owner,
				Project:  project,
				Name:     name,
			},
			kind: kind,
		}
	}
	return found, nil
}
