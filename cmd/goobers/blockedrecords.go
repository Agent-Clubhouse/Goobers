package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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

type parkedDependency struct {
	ItemID   string
	Blockers []string
}

type blockedEligibilitySkip struct {
	ItemID       string
	OpenBlockers []string
}

func (s blockedEligibilitySkip) reason() string {
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

func cloneBlockedRecords(recs map[string]blockedRecord) map[string]blockedRecord {
	cloned := make(map[string]blockedRecord, len(recs))
	for itemID, rec := range recs {
		rec.Blockers = append([]string(nil), rec.Blockers...)
		cloned[itemID] = rec
	}
	return cloned
}

func snapshotBlockedRecords(l instance.Layout) (map[string]blockedRecord, error) {
	var recs map[string]blockedRecord
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBacklogFilterBlocked, func() error {
		var err error
		recs, err = loadBlockedRecords(blockedRecordsPath(l))
		return err
	})
	return recs, err
}

func sameBlockedRecord(a, b blockedRecord) bool {
	return slices.Equal(a.Blockers, b.Blockers) &&
		a.RunID == b.RunID &&
		a.Stage == b.Stage &&
		a.Reason == b.Reason &&
		a.RecordedAt.Equal(b.RecordedAt)
}

// reconcileBlockedEligibilityLocked applies provider-resolved changes only to
// records that still match the snapshot, then filters against the latest map.
// The caller keeps claims.lock held through any subsequent ledger claim.
func reconcileBlockedEligibilityLocked(
	path string,
	eligible []providers.WorkItem,
	observed map[string]blockedRecord,
	resolved map[string]blockedRecord,
) ([]providers.WorkItem, []blockedEligibilitySkip, error) {
	current, err := loadBlockedRecords(path)
	if err != nil {
		return nil, nil, err
	}

	changed := false
	for itemID, observedRecord := range observed {
		currentRecord, ok := current[itemID]
		if !ok || !sameBlockedRecord(currentRecord, observedRecord) {
			continue
		}
		resolvedRecord, remains := resolved[itemID]
		if !remains {
			delete(current, itemID)
			changed = true
			continue
		}
		if !sameBlockedRecord(currentRecord, resolvedRecord) {
			current[itemID] = resolvedRecord
			changed = true
		}
	}
	if changed {
		if err := saveBlockedRecords(path, current); err != nil {
			return nil, nil, err
		}
	}

	filtered := make([]providers.WorkItem, 0, len(eligible))
	var skipped []blockedEligibilitySkip
	for _, item := range eligible {
		rec, blocked := current[item.ID]
		if !blocked {
			filtered = append(filtered, item)
			continue
		}
		blockers := append([]string(nil), rec.Blockers...)
		sort.Strings(blockers)
		skipped = append(skipped, blockedEligibilitySkip{ItemID: item.ID, OpenBlockers: blockers})
	}
	return filtered, skipped, nil
}

// filterBlockedEligibility removes from eligible any item with a recorded
// dependency block (#552) whose blockers are not all closed yet, so
// `implementation` skips known-blocked work instead of re-spending a full
// agentic attempt rediscovering the identical block every tick. It also keeps
// blocked.json from accumulating dead weight (QA-1's gate condition):
//
//   - Self-heal: closed blockers are pruned; once every one of a record's
//     blockers is closed, the record is cleared and the item is eligible
//     again — no human involved.
//   - Prune: a record whose OWN item is no longer open (closed by any path —
//     manual close, a downstream workflow, curation) is cleared outright,
//     since there is nothing left to skip or heal.
//
// GetWorkItem calls are memoized per call and happen before the caller enters
// the claim transaction. recs is mutated in place; changed reports whether the
// caller must persist it. A malformed item key degrades without blocking an
// unrelated candidate, while provider failures for valid records fail closed
// by leaving the item parked.
func filterBlockedEligibility(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, eligible []providers.WorkItem, recs map[string]blockedRecord) (filtered []providers.WorkItem, changed bool, warnings []string) {
	if len(recs) == 0 {
		return eligible, false, nil
	}

	openCache := map[string]bool{}
	isOpen := func(id string) (bool, error) {
		lookupID := blockedLookupID(id)
		if v, ok := openCache[lookupID]; ok {
			return v, nil
		}
		item, gerr := provider.GetWorkItem(ctx, repo, lookupID)
		if gerr != nil {
			return false, gerr
		}
		open := strings.EqualFold(item.State, "open")
		openCache[lookupID] = open
		return open, nil
	}
	validLookupID := func(id string) bool {
		_, err := strconv.ParseUint(blockedLookupID(id), 10, 64)
		return err == nil
	}

	skip := make(map[string]bool, len(recs))
	var lookupErrs []string
	itemIDs := make([]string, 0, len(recs))
	for itemID := range recs {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Strings(itemIDs)
	for _, itemID := range itemIDs {
		rec := recs[itemID]
		if !validLookupID(itemID) {
			lookupErrs = append(lookupErrs, fmt.Sprintf("check blocked item %s: invalid blocked-record key", itemID))
			continue
		}
		open, oerr := isOpen(itemID)
		if oerr != nil {
			lookupErrs = append(lookupErrs, fmt.Sprintf("check blocked item %s: %v", itemID, oerr))
			skip[itemID] = true
			continue
		}
		if !open {
			delete(recs, itemID)
			changed = true
			continue
		}

		unresolved := false
		blockerIDs := append([]string(nil), rec.Blockers...)
		sort.Strings(blockerIDs)
		openBlockers := make([]string, 0, len(blockerIDs))
		for _, blockerID := range blockerIDs {
			if !validLookupID(blockerID) {
				lookupErrs = append(lookupErrs, fmt.Sprintf("check blocker %s for %s: invalid blocked-record key", blockerID, itemID))
				unresolved = true
				break
			}
			blockerOpen, berr := isOpen(blockerID)
			if berr != nil {
				lookupErrs = append(lookupErrs, fmt.Sprintf("check blocker %s for %s: %v", blockerID, itemID, berr))
				unresolved = true
				break
			}
			if blockerOpen {
				openBlockers = append(openBlockers, blockerID)
			}
		}
		if unresolved {
			skip[itemID] = true
			continue
		}
		if len(openBlockers) == 0 {
			delete(recs, itemID)
			changed = true
			continue
		}
		if len(openBlockers) != len(rec.Blockers) {
			rec.Blockers = openBlockers
			recs[itemID] = rec
			changed = true
		}
		skip[itemID] = true
	}

	if len(skip) == 0 {
		return eligible, changed, lookupErrs
	}
	out := make([]providers.WorkItem, 0, len(eligible))
	for _, item := range eligible {
		if skip[item.ID] {
			continue
		}
		out = append(out, item)
	}
	return out, changed, lookupErrs
}

func resolveBlockedEligibility(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	eligible []providers.WorkItem,
) (observed, resolved map[string]blockedRecord, warnings []string, err error) {
	observed, err = snapshotBlockedRecords(l)
	if err != nil {
		return nil, nil, nil, err
	}
	resolved = cloneBlockedRecords(observed)
	_, _, warnings = filterBlockedEligibility(ctx, provider, repo, eligible, resolved)
	return observed, resolved, warnings, nil
}

func refreshBlockedEligibility(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	eligible []providers.WorkItem,
) ([]providers.WorkItem, error) {
	observed, resolved, _, err := resolveBlockedEligibility(ctx, l, provider, repo, eligible)
	if err != nil {
		return nil, err
	}
	var filtered []providers.WorkItem
	err = withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBacklogFilterBlocked, func() error {
		var reconcileErr error
		filtered, _, reconcileErr = reconcileBlockedEligibilityLocked(blockedRecordsPath(l), eligible, observed, resolved)
		return reconcileErr
	})
	return filtered, err
}

func listParkedDependencies(l instance.Layout) ([]parkedDependency, error) {
	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		return nil, err
	}
	parked := make([]parkedDependency, 0, len(recs))
	for itemID, rec := range recs {
		blockers := append([]string(nil), rec.Blockers...)
		sort.Strings(blockers)
		parked = append(parked, parkedDependency{ItemID: itemID, Blockers: blockers})
	}
	sort.Slice(parked, func(i, j int) bool {
		return parked[i].ItemID < parked[j].ItemID
	})
	return parked, nil
}

// updateBlockedRecords applies fn to the records map under the instance's
// claim lock (blocked.json shares claims.lock rather than growing a second
// lock file — writers are the same claim-lifecycle actors) and persists the
// result. fn returns false to skip the write (nothing changed).
func updateBlockedRecords(l instance.Layout, fn func(recs map[string]blockedRecord) bool) error {
	path := blockedRecordsPath(l)
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBlockedUpdate, func() error {
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
