package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// Sibling-context cache (issue #523): gather-sibling-context's durable
// per-sibling memo, so consecutive merge-review runs stop re-fetching every
// other open PR's files from scratch. Check state remains in the entry as the
// latest observation but is refreshed every run because CI can be rerun on an
// unchanged SHA.
// Lives under the instance's scheduler dir next to claims.json, guarded by
// the same cross-process flock pattern (withFileLock): concurrent
// merge-review runs' gather stages each run as their own OS process against
// the same instance root.
const (
	siblingCacheFileName     = stateclient.KeySiblingContextCache
	siblingCacheLockFileName = "sibling-context-cache.lock"
)

// siblingCacheEntry is one sibling PR's memoized gather output, keyed by the
// PR number and pinned to the head SHA it was gathered at. Files can only
// change when the head SHA does, so a SHA match makes them reusable as-is.
// CheckState records the latest observation for compatibility with existing
// cache files, but gather-sibling-context refreshes it before use.
type siblingCacheEntry struct {
	HeadSHA    string               `json:"headSha"`
	CheckState providers.CheckState `json:"checkState"`
	Files      []string             `json:"files"`
	// Lines is the total changed-line count (sum of every file's
	// Additions+Deletions) as of HeadSHA — the #1313 scope-gate's second
	// magnitude, computed from the same PullRequestFiles call Files already
	// comes from (no extra fetch). Reusable whenever Files is (a HeadSHA
	// match), for every PR regardless of whether it's the selected PR or a
	// sibling in this run — a PR gathered as a sibling today may be
	// selected in a later run, and that later run's cache hit must not
	// silently see a stale/zero Lines value.
	Lines int `json:"lines"`
}

// siblingCacheFile is the cache's on-disk envelope. Entries is keyed by the
// PR number as a string (JSON object keys), pruned on every save to the
// currently-open sibling set so closed/merged PRs don't accumulate.
type siblingCacheFile struct {
	Entries map[string]siblingCacheEntry `json:"entries"`
}

// loadSiblingCache reads the cache through the scheduler-state seam. It never
// fails the stage: a missing key is the normal first-run outcome, and an
// unreadable/corrupt one degrades to an empty cache (a full fresh gather)
// with a warning — the cache is an optimization, never a correctness input.
//
// Since #3878 this reaches the daemon's copy over the scheduler-state plane
// when the stage runs in a pod, so a gather in a pod hits the memo a previous
// gather populated instead of starting from an empty pod-local file every run.
func loadSiblingCache(l instance.Layout, stderr io.Writer) map[string]siblingCacheEntry {
	entries, err := readSiblingCache(l)
	if err != nil {
		pf(stderr, "warning: sibling-context cache unreadable, gathering fresh: %v\n", err)
		return nil
	}
	return entries
}

func readSiblingCache(l instance.Layout) (map[string]siblingCacheEntry, error) {
	store, err := openSiblingCacheStore(l)
	if err != nil {
		return nil, err
	}
	value, err := store.Get(stateContext(), stateclient.KeySiblingContextCache)
	if err != nil {
		return nil, err
	}
	return decodeSiblingCache(value)
}

func decodeSiblingCache(value stateclient.Value) (map[string]siblingCacheEntry, error) {
	if !value.Exists() {
		return nil, nil
	}
	var f siblingCacheFile
	if err := json.Unmarshal(value.Data, &f); err != nil {
		return nil, err
	}
	return f.Entries, nil
}

// saveSiblingCache writes entries back through the scheduler-state seam.
// Errors are the caller's to report-and-continue: failing to persist the memo
// must not fail a gather that already succeeded.
//
// The write is unconditional rather than a compare-and-swap merge: the memo is
// pruned to the currently-open sibling set on every save, so the last writer's
// view is the correct one and a "lost" concurrent update costs at most one
// re-gather. Correctness never depends on it.
func saveSiblingCache(l instance.Layout, entries map[string]siblingCacheEntry) error {
	store, err := openSiblingCacheStore(l)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(siblingCacheFile{Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sibling-context cache: %w", err)
	}
	return store.Update(stateContext(), stateclient.KeySiblingContextCache, stateLockOperationSiblingUpdate,
		func(stateclient.Value) ([]byte, bool, error) {
			return data, true, nil
		})
}

// openSiblingCacheStore builds the sibling-cache store, creating the scheduler
// directory for a standalone/manual invocation against a root that was never
// scaffolded. A stage pod creates nothing: the plane owns the daemon's
// scheduler directory.
func openSiblingCacheStore(l instance.Layout) (stateclient.Store, error) {
	if !statePlaneSelected() {
		if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
			return nil, err
		}
	}
	return openStageStateStore(l)
}
