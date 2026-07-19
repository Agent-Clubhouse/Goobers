package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// blockedRecordsFileName is the well-known file under an instance's
// scheduler/ directory recording learned dependency blocks (#552): items a
// prior run reported blocked on still-open issues, so backlog selection can
// skip them instead of re-spending a full agentic attempt rediscovering the
// identical block every tick. Sibling to claims.json and guarded by the same
// claims.lock — records are written by the runner's blocked handler and
// cleared by backlog-query's self-heal once every recorded blocker closes.
const blockedRecordsFileName = "blocked.json"

// blockedRecord is one learned dependency block: the issue numbers the item
// was reported blocked on, plus provenance for a human inspecting the file.
type blockedRecord struct {
	Blockers   []string  `json:"blockers"`
	RunID      string    `json:"runId"`
	Stage      string    `json:"stage,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	RecordedAt time.Time `json:"recordedAt"`
}

type blockedEligibilitySkip struct {
	ItemID              string
	ItemStateUnresolved bool
	OpenBlockers        []string
	UnresolvedBlockers  []string
	VerificationPending bool
	record              blockedRecord
}

func (s blockedEligibilitySkip) reason() string {
	if s.ItemStateUnresolved {
		return fmt.Sprintf("learned block: item %s parked; item state unresolved", s.ItemID)
	}
	if s.VerificationPending {
		return fmt.Sprintf("learned block: item %s parked; blocked record changed during eligibility check", s.ItemID)
	}
	if len(s.UnresolvedBlockers) != 0 {
		return fmt.Sprintf(
			"learned block: item %s parked; blocker state unresolved: %s",
			s.ItemID,
			strings.Join(s.UnresolvedBlockers, ","),
		)
	}
	return fmt.Sprintf("learned block: item %s parked on open blocker(s): %s", s.ItemID, strings.Join(s.OpenBlockers, ","))
}

func blockedRecordsPath(l instance.Layout) string {
	return filepath.Join(l.SchedulerDir(), blockedRecordsFileName)
}

// blockedLookupID converts a blocked.json key into the id a provider lookup
// expects. Keys come from whatever the claim ledger used for the run's
// driving item, and the blocked handler is deliberately workflow-agnostic
// (runnerwiring.go's buildBlockedHandler), so a pr-remediation run records
// its claim name — "pr/955" — while issue-driven runs record a bare "955".
//
// GetWorkItem builds its URL as .../issues/{id} literally, so a "pr/"-
// prefixed id produced .../issues/pr/955: an invalid path, a 404, and (before
// this was handled) a hard failure of every query-backlog tick — which took
// down every workflow whose first stage is query-backlog, implementation and
// backlog-curation alike (#971). Stripping the prefix is correct rather than
// merely expedient: GitHub numbers issues and pull requests in one shared
// sequence and serves both at /issues/{number}, so the bare number resolves
// the pull request, and its state drives the same self-heal/prune logic every
// other record gets.
func blockedLookupID(key string) string {
	return strings.TrimPrefix(key, pullRequestClaimPrefix)
}

// loadBlockedRecords reads the records map; a missing file is an empty map
// (the overwhelmingly common steady state), never an error.
func loadBlockedRecords(path string) (map[string]blockedRecord, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]blockedRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	recs := map[string]blockedRecord{}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return recs, nil
}

func saveBlockedRecords(path string, recs map[string]blockedRecord) error {
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal blocked records: %w", err)
	}
	// Write-then-rename for the same torn-write reason the claim ledger's own
	// persistence uses: a crash mid-write must never leave a half-written
	// file that fails every subsequent selection tick's parse.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

func snapshotBlockedRecords(l instance.Layout) (map[string]blockedRecord, error) {
	var recs map[string]blockedRecord
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), func() error {
		var err error
		recs, err = loadBlockedRecords(blockedRecordsPath(l))
		return err
	})
	return recs, err
}

// reconcileBlockedEligibilityLocked applies provider-resolved removals to the
// latest blocked-record state, then excludes every item that is still recorded
// as blocked. verifiedSkips contains provider-backed reasons for records from
// the earlier snapshot. A concurrent addition or replacement stays parked for
// this cycle because its blocker state has not been verified. The caller must
// hold claims.lock through any subsequent claim so a blocked-record update
// cannot race the eligibility decision.
func reconcileBlockedEligibilityLocked(path string, eligible []providers.WorkItem, resolved map[string]blockedRecord, verifiedSkips map[string]blockedEligibilitySkip) ([]providers.WorkItem, []blockedEligibilitySkip, error) {
	current, err := loadBlockedRecords(path)
	if err != nil {
		return nil, nil, err
	}

	changed := false
	for itemID, observed := range resolved {
		record, ok := current[itemID]
		if !ok || !sameBlockedRecord(record, observed) {
			continue
		}
		delete(current, itemID)
		changed = true
	}
	if changed {
		if err := saveBlockedRecords(path, current); err != nil {
			return nil, nil, err
		}
	}

	if len(current) == 0 {
		return eligible, nil, nil
	}
	filtered := eligible[:0]
	var skipped []blockedEligibilitySkip
	for _, item := range eligible {
		record, blocked := current[item.ID]
		if !blocked {
			filtered = append(filtered, item)
			continue
		}
		if skip, ok := verifiedSkips[item.ID]; ok && sameBlockedRecord(record, skip.record) {
			skipped = append(skipped, skip)
			continue
		}
		skipped = append(skipped, blockedEligibilitySkip{
			ItemID:              item.ID,
			VerificationPending: true,
			record:              record,
		})
	}
	return filtered, skipped, nil
}

func sameBlockedRecord(a, b blockedRecord) bool {
	return slices.Equal(a.Blockers, b.Blockers) &&
		a.RunID == b.RunID &&
		a.Stage == b.Stage &&
		a.Reason == b.Reason &&
		a.RecordedAt.Equal(b.RecordedAt)
}

// filterBlockedEligibility removes from eligible any item with a recorded
// dependency block (#552) whose blockers are not all closed yet, so
// `implementation` skips known-blocked work instead of re-spending a full
// agentic attempt rediscovering the identical block every tick. It also keeps
// blocked.json from accumulating dead weight (QA-1's gate condition):
//
//   - Self-heal: once every one of a record's blockers is closed, the record
//     is cleared and the item is eligible again — no human involved.
//   - Prune: a record whose OWN item is no longer open (closed by any path —
//     manual close, a downstream workflow, curation) is cleared outright,
//     since there is nothing left to skip or heal.
//
// GetWorkItem calls are memoized per call (issue ids repeat across records/
// blockers) and scoped to just the recorded items/blockers — a small,
// bounded set proportional to how many items are CURRENTLY blocked, never to
// backlog size. A lookup failure affects only its record: unresolved items
// stay untouched, unresolved blockers keep their item parked, and resolvable
// records continue to self-heal or prune. The caller surfaces returned
// warnings so malformed records remain visible without stalling every tick.
func filterBlockedEligibility(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, eligible []providers.WorkItem, recs map[string]blockedRecord) (filtered []providers.WorkItem, skipped []blockedEligibilitySkip, changed bool, warnings []string) {
	if len(recs) == 0 {
		return eligible, nil, false, nil
	}

	openCache := map[string]bool{}
	isOpen := func(id string) (bool, error) {
		if v, ok := openCache[id]; ok {
			return v, nil
		}
		item, gerr := provider.GetWorkItem(ctx, repo, blockedLookupID(id))
		if gerr != nil {
			return false, gerr
		}
		open := strings.EqualFold(item.State, "open")
		openCache[id] = open
		return open, nil
	}

	itemIDs := make([]string, 0, len(recs))
	for itemID := range recs {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Strings(itemIDs)
	eligibleIDs := make(map[string]bool, len(eligible))
	for _, item := range eligible {
		eligibleIDs[item.ID] = true
	}

	skip := make(map[string]bool, len(recs))
	var remove []string
	var lookupWarnings []string
	for _, itemID := range itemIDs {
		rec := recs[itemID]
		open, oerr := isOpen(itemID)
		if oerr != nil {
			lookupWarnings = append(lookupWarnings, fmt.Sprintf("check blocked item %s: %v", itemID, oerr))
			if eligibleIDs[itemID] {
				skip[itemID] = true
				skipped = append(skipped, blockedEligibilitySkip{
					ItemID:              itemID,
					ItemStateUnresolved: true,
					record:              rec,
				})
			}
			continue
		}
		if !open {
			remove = append(remove, itemID)
			continue
		}

		blockerIDs := append([]string(nil), rec.Blockers...)
		sort.Strings(blockerIDs)
		var openBlockers []string
		var unresolvedBlockers []string
		for _, blockerID := range blockerIDs {
			blockerOpen, berr := isOpen(blockerID)
			if berr != nil {
				lookupWarnings = append(lookupWarnings, fmt.Sprintf("check blocker %s for %s: %v", blockerID, itemID, berr))
				unresolvedBlockers = append(unresolvedBlockers, blockerID)
				continue
			}
			if blockerOpen {
				openBlockers = append(openBlockers, blockerID)
			}
		}
		if len(unresolvedBlockers) != 0 {
			if eligibleIDs[itemID] {
				skip[itemID] = true
				skipped = append(skipped, blockedEligibilitySkip{
					ItemID:             itemID,
					OpenBlockers:       openBlockers,
					UnresolvedBlockers: unresolvedBlockers,
					record:             rec,
				})
			}
			continue
		}
		if len(openBlockers) == 0 {
			remove = append(remove, itemID)
			continue
		}
		if eligibleIDs[itemID] {
			skip[itemID] = true
			skipped = append(skipped, blockedEligibilitySkip{ItemID: itemID, OpenBlockers: openBlockers, record: rec})
		}
	}

	for _, itemID := range remove {
		delete(recs, itemID)
		changed = true
	}

	if len(skip) == 0 {
		return eligible, skipped, changed, lookupWarnings
	}
	out := eligible[:0]
	for _, item := range eligible {
		if skip[item.ID] {
			continue
		}
		out = append(out, item)
	}
	return out, skipped, changed, lookupWarnings
}

// updateBlockedRecords applies fn to the records map under the instance's
// claim lock (blocked.json shares claims.lock rather than growing a second
// lock file — writers are the same claim-lifecycle actors) and persists the
// result. fn returns false to skip the write (nothing changed).
func updateBlockedRecords(l instance.Layout, fn func(recs map[string]blockedRecord) bool) error {
	path := blockedRecordsPath(l)
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), func() error {
		recs, err := loadBlockedRecords(path)
		if err != nil {
			return err
		}
		if !fn(recs) {
			return nil
		}
		return saveBlockedRecords(path, recs)
	})
}
