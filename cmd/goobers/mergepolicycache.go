package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/providers"
)

// Merge-policy detection cache (issue #758): detecting a repo's active
// merge policy (branch protection/ruleset state) is a live provider call,
// so `merge-pr` memoizes it — like the sibling-context cache (#523) — under
// the instance's scheduler dir, guarded by the same cross-process flock
// pattern (withClaimLock), since concurrent merge-review runs' merge-pr
// stages each run as their own OS process against the same instance root.
//
// Unlike the sibling-context cache, a stale merge policy is a correctness
// input, not just an optimization: internal/mergepolicy's Land dispatches
// directly on it. A short TTL bounds the staleness window (a real policy
// flip, e.g. #631/#759 enabling merge queue on Goobers' own repo, is picked
// up within one operator-observable window) while still sparing a
// maxConcurrentRuns:4 merge-review from re-querying the provider on every
// single run.
const (
	mergePolicyCacheFileName     = "merge-policy-cache.json"
	mergePolicyCacheLockFileName = "merge-policy-cache.lock"
	mergePolicyCacheTTL          = 10 * time.Minute
)

// mergePolicyCacheEntry is one repo+branch's memoized detection result.
type mergePolicyCacheEntry struct {
	Policy     providers.MergePolicy `json:"policy"`
	DetectedAt time.Time             `json:"detectedAt"`
}

// mergePolicyCacheFile is the cache's on-disk envelope, keyed by
// mergePolicyCacheKey.
type mergePolicyCacheFile struct {
	Entries map[string]mergePolicyCacheEntry `json:"entries"`
}

// mergePolicyCacheKey identifies one repo+branch's cached policy.
func mergePolicyCacheKey(repo providers.RepositoryRef, branch string) string {
	return repo.Owner + "/" + repo.Name + "@" + branch
}

// detectMergePolicy resolves repo's active merge policy for branch, from
// the cache when a fresh-enough entry exists, else via a live
// provider.DetectMergePolicy call whose result is then persisted for
// subsequent callers. A cache miss/expiry/corruption never fails the
// caller differently than a genuine detection failure would — it just
// means a live call happens now instead of being skipped.
func detectMergePolicy(ctx context.Context, provider providers.RepoProvider, schedulerDir string, repo providers.RepositoryRef, branch string, stderr io.Writer) (providers.MergePolicy, error) {
	key := mergePolicyCacheKey(repo, branch)
	if entry, ok := loadMergePolicyCacheEntry(schedulerDir, key, stderr); ok {
		return entry.Policy, nil
	}
	result, err := provider.DetectMergePolicy(ctx, providers.RepoMergePolicyRequest{Repository: repo, Branch: branch})
	if err != nil {
		return "", err
	}
	if err := saveMergePolicyCacheEntry(schedulerDir, key, mergePolicyCacheEntry{Policy: result.Policy, DetectedAt: time.Now()}); err != nil {
		pf(stderr, "warning: persist merge-policy cache: %v\n", err)
	}
	return result.Policy, nil
}

// loadMergePolicyCacheEntry reads key's cache entry under the cross-process
// lock, treating a missing file, unreadable/corrupt file, missing key, or
// expired entry all as a uniform cache miss (ok=false) — the cache is
// consulted opportunistically, never required for correctness (a miss just
// means detectMergePolicy falls back to a live provider call).
func loadMergePolicyCacheEntry(schedulerDir, key string, stderr io.Writer) (mergePolicyCacheEntry, bool) {
	path := filepath.Join(schedulerDir, mergePolicyCacheFileName)
	var entries map[string]mergePolicyCacheEntry
	err := withMergePolicyCacheLock(schedulerDir, func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		var f mergePolicyCacheFile
		if err := json.Unmarshal(data, &f); err != nil {
			return err
		}
		entries = f.Entries
		return nil
	})
	if err != nil {
		pf(stderr, "warning: merge-policy cache unreadable, re-detecting: %v\n", err)
		return mergePolicyCacheEntry{}, false
	}
	entry, ok := entries[key]
	if !ok || time.Since(entry.DetectedAt) > mergePolicyCacheTTL {
		return mergePolicyCacheEntry{}, false
	}
	return entry, true
}

// saveMergePolicyCacheEntry upserts key's entry under the cross-process
// lock (read-modify-write in one lock hold, so two concurrent detectors for
// DIFFERENT repos/branches never lose each other's entry) via
// temp-file-plus-rename so a concurrent load never sees a torn write.
func saveMergePolicyCacheEntry(schedulerDir, key string, entry mergePolicyCacheEntry) error {
	path := filepath.Join(schedulerDir, mergePolicyCacheFileName)
	return withMergePolicyCacheLock(schedulerDir, func() error {
		entries := map[string]mergePolicyCacheEntry{}
		if data, err := os.ReadFile(path); err == nil {
			var f mergePolicyCacheFile
			if jerr := json.Unmarshal(data, &f); jerr == nil && f.Entries != nil {
				entries = f.Entries
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		entries[key] = entry
		out, err := json.MarshalIndent(mergePolicyCacheFile{Entries: entries}, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal merge-policy cache: %w", err)
		}
		tmp, err := os.CreateTemp(schedulerDir, mergePolicyCacheFileName+".tmp-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		if _, err := tmp.Write(out); err != nil {
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

// withMergePolicyCacheLock serializes cache reads/writes across concurrent
// merge-pr processes, reusing withClaimLock's blocking-flock discipline
// (siblingcache.go's withSiblingCacheLock rationale applies identically
// here: each stage dispatch is its own OS process, so an in-process mutex
// cannot arbitrate). Creates schedulerDir if a standalone/manual invocation
// runs against a root that was never scaffolded.
func withMergePolicyCacheLock(schedulerDir string, fn func() error) error {
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		return err
	}
	return withClaimLock(filepath.Join(schedulerDir, mergePolicyCacheLockFileName), fn)
}
