package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func blockedRecordsPath(l instance.Layout) string {
	return filepath.Join(l.SchedulerDir(), blockedRecordsFileName)
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
// backlog size. recs is mutated in place; changed reports whether the caller
// must persist it.
func filterBlockedEligibility(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, eligible []providers.WorkItem, recs map[string]blockedRecord) (filtered []providers.WorkItem, changed bool, err error) {
	if len(recs) == 0 {
		return eligible, false, nil
	}

	openCache := map[string]bool{}
	isOpen := func(id string) (bool, error) {
		if v, ok := openCache[id]; ok {
			return v, nil
		}
		item, gerr := provider.GetWorkItem(ctx, repo, id)
		if gerr != nil {
			return false, gerr
		}
		open := strings.EqualFold(item.State, "open")
		openCache[id] = open
		return open, nil
	}

	skip := make(map[string]bool, len(recs))
	for itemID, rec := range recs {
		open, oerr := isOpen(itemID)
		if oerr != nil {
			return nil, false, fmt.Errorf("check blocked item %s: %w", itemID, oerr)
		}
		if !open {
			delete(recs, itemID)
			changed = true
			continue
		}

		allClosed := true
		for _, blockerID := range rec.Blockers {
			blockerOpen, berr := isOpen(blockerID)
			if berr != nil {
				return nil, false, fmt.Errorf("check blocker %s for %s: %w", blockerID, itemID, berr)
			}
			if blockerOpen {
				allClosed = false
				break
			}
		}
		if allClosed {
			delete(recs, itemID)
			changed = true
			continue
		}
		skip[itemID] = true
	}

	if len(skip) == 0 {
		return eligible, changed, nil
	}
	out := eligible[:0]
	for _, item := range eligible {
		if skip[item.ID] {
			continue
		}
		out = append(out, item)
	}
	return out, changed, nil
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
