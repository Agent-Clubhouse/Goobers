package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// objectCacheDirName is the node-level object cache's directory name under
// Manager.pinnedRoot (design §3 B3, issue #654): the cache for repo key k
// lives at "<pinnedRoot>/_objects/<k>".
const objectCacheDirName = "_objects"

// alternatesRelPath is where git records a bare repository's alternate
// object directories, relative to the repository's own directory.
const alternatesRelPath = "objects/info/alternates"

func (m *Manager) objectCacheDirForKey(key string) string {
	return filepath.Join(m.pinnedRoot, objectCacheDirName, key)
}

func (m *Manager) objectCacheLockPath(key string) string {
	return filepath.Join(m.pinnedRoot, objectCacheDirName, key+".lock")
}

// ensureObjectCache clones (first use) or fetches (thereafter) the node-level
// bare mirror cache for repoURL, returning its directory.
//
// Guarded by a cross-process file lock (internal/platform/lock), not
// Manager.lockFor: the cache is shared across every gaggle's Manager on the
// node (Manager.pinnedRoot is the same node-wide directory regardless of
// which gaggle constructed the Manager — see WithPinnedRoot), and those are
// typically distinct Manager values with independent, unshared in-memory
// repoLocks maps. A caller already holds lockFor(key) for its own
// dependent-mirror creation, which is what serializes "ensure the cache,
// then reference it" within one Manager; the file lock here is what makes
// the cache's own clone/fetch itself safe against a DIFFERENT gaggle's
// Manager doing the same thing concurrently.
//
// The cache is always a full mirror clone, regardless of WithPartialClone: a
// blobless cache could not satisfy an alternates lookup for a blob it never
// fetched, defeating the point of referencing it.
func (m *Manager) ensureObjectCache(ctx context.Context, repoURL, key string) (string, error) {
	dir := m.objectCacheDirForKey(key)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", fmt.Errorf("worktree: create object cache parent for %s: %w", repoURL, err)
	}
	handle, err := acquireObjectCacheLock(m.objectCacheLockPath(key))
	if err != nil {
		return "", fmt.Errorf("worktree: acquire object cache lock for %s: %w", repoURL, err)
	}
	defer func() { _ = handle.Release() }()

	switch _, statErr := os.Stat(dir); {
	case os.IsNotExist(statErr):
		if err := m.runRemoteGit(ctx, repoURL, "", "clone", "--mirror", repoURL, dir); err != nil {
			_ = os.RemoveAll(dir) // don't leave a partial cache masquerading as a valid one
			return "", fmt.Errorf("worktree: clone object cache for %s: %w", repoURL, err)
		}
		return dir, nil
	case statErr != nil:
		return "", fmt.Errorf("worktree: stat object cache for %s: %w", repoURL, statErr)
	}
	if err := m.runRemoteGit(ctx, repoURL, dir, "fetch", "--prune", "origin", "+refs/*:refs/*"); err != nil {
		return "", fmt.Errorf("worktree: fetch object cache for %s: %w", repoURL, err)
	}
	return dir, nil
}

// GCObjectCacheResult reports one repo key's garbage-collection outcome.
type GCObjectCacheResult struct {
	// Deleted reports whether the cache directory was removed.
	Deleted bool
	// Dependents lists every mirror directory (repo.git) whose alternates
	// still reference this cache, in sorted order. Non-empty exactly when
	// Deleted is false and the cache directory exists.
	Dependents []string
}

// GCObjectCache deletes the node-level object cache for repoURL if, and only
// if, no mirror under any of workcopiesRoots still references it via
// objects/info/alternates (design §3 B3). Fail-closed: any dependent, or any
// error walking or reading an alternates file, refuses deletion rather than
// risk removing a cache a live mirror still borrows objects from. A cache
// directory that does not exist is reported as Deleted=false with no error
// and no dependents — there is nothing to collect.
//
// workcopiesRoots should be every workcopies directory on the node the
// caller wants checked for dependents — ordinarily every gaggle's
// instance.Layout.WorkcopiesDirs() plus the legacy/default root, so a mirror
// in any gaggle is found regardless of which gaggle's Manager originally
// created it.
//
// There is no automatic/background GC (out of scope for this iteration,
// design §3 B3): this is meant to be invoked deliberately by an operator.
func (m *Manager) GCObjectCache(repoURL string, workcopiesRoots []string) (GCObjectCacheResult, error) {
	key := repoKey(repoURL)
	cacheDir := m.objectCacheDirForKey(key)
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		return GCObjectCacheResult{}, fmt.Errorf("worktree: create object cache parent for %s: %w", repoURL, err)
	}
	cacheObjectsDir := filepath.Clean(filepath.Join(cacheDir, "objects"))
	// git resolves symlinks (e.g. macOS's /var -> /private/var) when it
	// normalizes the --reference path it writes into a dependent's
	// alternates file, so the comparison target must be resolved the same
	// way — comparing against the unresolved path here would silently find
	// zero dependents for every mirror that DOES depend on this cache,
	// which for a fail-closed GC is the one outcome that must never happen.
	// A cache dir that doesn't exist (nothing ever cloned, or already
	// removed) has nothing to resolve; keep the unresolved path in that
	// case — no alternates entry could match it either way.
	if resolved, err := filepath.EvalSymlinks(cacheObjectsDir); err == nil {
		cacheObjectsDir = resolved
	}

	handle, err := acquireObjectCacheLock(m.objectCacheLockPath(key))
	if err != nil {
		return GCObjectCacheResult{}, fmt.Errorf("worktree: acquire object cache lock for %s: %w", repoURL, err)
	}
	defer func() { _ = handle.Release() }()

	var dependents []string
	for _, root := range workcopiesRoots {
		found, err := mirrorsReferencingObjectsDir(root, cacheObjectsDir)
		if err != nil {
			return GCObjectCacheResult{}, fmt.Errorf("worktree: scan %s for object cache dependents: %w", root, err)
		}
		dependents = append(dependents, found...)
	}
	if len(dependents) > 0 {
		sort.Strings(dependents)
		return GCObjectCacheResult{Dependents: dependents}, nil
	}

	if _, err := os.Stat(cacheDir); errors.Is(err, os.ErrNotExist) {
		return GCObjectCacheResult{}, nil
	} else if err != nil {
		return GCObjectCacheResult{}, fmt.Errorf("worktree: stat object cache for %s: %w", repoURL, err)
	}
	if err := os.RemoveAll(cacheDir); err != nil {
		return GCObjectCacheResult{}, fmt.Errorf("worktree: delete object cache for %s: %w", repoURL, err)
	}
	return GCObjectCacheResult{Deleted: true}, nil
}

// mirrorsReferencingObjectsDir walks root — a workcopies directory shaped
// "<root>/<repo-key>/repo.git" (Manager.repoDirForKey) — for every mirror
// whose objects/info/alternates lists cacheObjectsDir, returning their
// repo.git paths.
func mirrorsReferencingObjectsDir(root, cacheObjectsDir string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		mirrorDir := filepath.Join(root, entry.Name(), "repo.git")
		alternates, err := readAlternates(filepath.Join(mirrorDir, alternatesRelPath))
		if err != nil {
			return nil, fmt.Errorf("read alternates for %s: %w", mirrorDir, err)
		}
		for _, alt := range alternates {
			if alt == cacheObjectsDir {
				found = append(found, mirrorDir)
				break
			}
		}
	}
	return found, nil
}

// readAlternates parses a git objects/info/alternates file: one
// object-directory path per line, blank lines and #-comments ignored. A path
// not already absolute is resolved relative to the alternates file's own
// directory, matching git's own interpretation (a relative entry is relative
// to "objects/info/", i.e. two directories up from the entry itself) — not
// the caller's current working directory. A missing file is not an error; it
// just means this mirror references nothing.
//
// This does not handle C-quoted alternates lines (a path containing
// characters needing escaping) — every alternates entry this codebase writes
// is a plain absolute path produced by git itself via `--reference` at clone
// time, which git never quotes.
func readAlternates(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !filepath.IsAbs(line) {
			line = filepath.Join(dir, line)
		}
		out = append(out, filepath.Clean(line))
	}
	return out, nil
}
