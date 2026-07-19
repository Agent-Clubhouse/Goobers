package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/providers"
)

// Sibling-context cache (issue #523): gather-sibling-context's durable
// per-sibling memo, so consecutive merge-review runs stop re-fetching every
// other open PR's files + check state from scratch (the per-PR cost — one
// files request plus two check-state requests — dominated the instance's
// GitHub API budget as the open-PR set grew, #614's rate-limit exhaustion).
// Lives under the instance's scheduler dir next to claims.json, guarded by
// the same cross-process flock pattern (withClaimLock): concurrent
// merge-review runs' gather stages each run as their own OS process against
// the same instance root.
const (
	siblingCacheFileName     = "sibling-context-cache.json"
	siblingCacheLockFileName = "sibling-context-cache.lock"
)

// siblingCacheEntry is one sibling PR's memoized gather output, keyed by the
// PR number and pinned to the head SHA it was gathered at. Files can only
// change when the head SHA does, so a SHA match makes them reusable as-is;
// CheckState can advance on an unchanged SHA (CI finishing later), so only a
// terminal state (passing/failing) is reusable — a pending one is re-polled
// each run until it settles. A terminal state overwritten by a re-run on the
// same SHA goes stale here: an accepted tradeoff for sibling *evidence*
// (nothing gates a merge on it — pr-select and the auto-merge re-poll still
// resolve fresh state for the PR actually being acted on).
type siblingCacheEntry struct {
	HeadSHA    string               `json:"headSha"`
	CheckState providers.CheckState `json:"checkState"`
	Files      []string             `json:"files"`
}

// siblingCacheFile is the cache's on-disk envelope. Entries is keyed by the
// PR number as a string (JSON object keys), pruned on every save to the
// currently-open sibling set so closed/merged PRs don't accumulate.
type siblingCacheFile struct {
	Entries map[string]siblingCacheEntry `json:"entries"`
}

// checkStateTerminal reports whether s is settled enough to reuse across
// runs without re-polling.
func checkStateTerminal(s providers.CheckState) bool {
	return s == providers.CheckStatePassing || s == providers.CheckStateFailing
}

// loadSiblingCache reads the cache under the cross-process lock. It never
// fails the stage: a missing file is the normal first-run outcome, and an
// unreadable/corrupt one degrades to an empty cache (a full fresh gather)
// with a warning — the cache is an optimization, never a correctness input.
func loadSiblingCache(schedulerDir string, stderr io.Writer) map[string]siblingCacheEntry {
	path := filepath.Join(schedulerDir, siblingCacheFileName)
	var entries map[string]siblingCacheEntry
	err := withSiblingCacheLock(schedulerDir, func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		var f siblingCacheFile
		if err := json.Unmarshal(data, &f); err != nil {
			return err
		}
		entries = f.Entries
		return nil
	})
	if err != nil {
		pf(stderr, "warning: sibling-context cache unreadable, gathering fresh: %v\n", err)
		return nil
	}
	return entries
}

// saveSiblingCache writes entries back under the cross-process lock, via
// temp-file-plus-rename so a concurrent load never sees a torn write. Errors
// are the caller's to report-and-continue: failing to persist the memo must
// not fail a gather that already succeeded.
func saveSiblingCache(schedulerDir string, entries map[string]siblingCacheEntry) error {
	path := filepath.Join(schedulerDir, siblingCacheFileName)
	data, err := json.MarshalIndent(siblingCacheFile{Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sibling-context cache: %w", err)
	}
	return withSiblingCacheLock(schedulerDir, func() error {
		tmp, err := os.CreateTemp(schedulerDir, siblingCacheFileName+".tmp-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		if _, err := tmp.Write(data); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return err
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
		return os.Rename(tmpName, path)
	})
}

// withSiblingCacheLock serializes cache reads/writes across concurrent
// gather processes, reusing withBlockingFileLock's blocking-flock discipline (and
// claims.json's rationale: each stage dispatch is its own OS process, so an
// in-process mutex cannot arbitrate). Creates schedulerDir if a standalone/
// manual invocation runs against a root that was never scaffolded.
func withSiblingCacheLock(schedulerDir string, fn func() error) error {
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		return err
	}
	return withBlockingFileLock(filepath.Join(schedulerDir, siblingCacheLockFileName), nil, nil, fn)
}
