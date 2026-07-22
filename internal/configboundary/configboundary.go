// Package configboundary enforces the Tutor's config-only write-boundary
// (#104/T4, wired into the real open-pr stage by #223): every path a Tutor
// cycle would change must be contained within the instance's configured config
// root, so the self-improvement loop can never author a change to platform
// code, CI, or credentials — only config.
//
// This is the path-scoped half of the boundary. The structural half — a
// credential that cannot push platform changes even if this check were bypassed
// — is deferred to #35. Governance (CODEOWNERS/branch protection on the config
// root, from #104's PR) is repo config and lives alongside it.
//
// The containment test is lexical, mirroring api/v1alpha1.ResolveContainedPath
// (#120): absolute paths and any path that escapes its root via ".." are
// refused. It takes repo-relative paths (e.g. from `git diff --name-only`)
// rather than a provider CommitFile set, so it plugs into the real git-worktree
// open-pr architecture.
package configboundary

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrOutsideConfigRoot marks a change that would write outside the configured
// config root. Callers fail the cycle closed on it.
var ErrOutsideConfigRoot = errors.New("configboundary: path escapes configured config root")

// ErrInvalidDocsRoot marks a declared docs root that is not a usable
// repo-relative containment root. Config-load validation reports it as an error
// (#1016) rather than silently normalizing a bogus root to whole-repo the way
// the runtime boundary does.
var ErrInvalidDocsRoot = errors.New("configboundary: invalid docs root")

// ValidateDocsRoot reports whether a declared docs root is a usable containment
// root (#1016): non-empty, repo-relative (not absolute), naming a real subtree
// or file (not "." — the whole repo, which would defeat confinement), and not
// escaping the repository via "..". This is the config-load lexical check; a
// root's existence in the repository is a separate, filesystem check `goobers
// validate` layers on top.
func ValidateDocsRoot(root string) error {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return fmt.Errorf("%w: empty", ErrInvalidDocsRoot)
	}
	if rootedOrVolumeBound(trimmed) {
		return fmt.Errorf("%w: %q is absolute (roots are repo-relative)", ErrInvalidDocsRoot, root)
	}
	clean := filepath.Clean(strings.Trim(trimmed, "/"))
	if clean == "." {
		return fmt.Errorf("%w: %q names the whole repository, which would defeat confinement", ErrInvalidDocsRoot, root)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q escapes the repository", ErrInvalidDocsRoot, root)
	}
	return nil
}

// Confine returns nil only when every path in changed is contained within
// configRoot. configRoot is the instance-configured, repo-relative config root
// (e.g. "selfhost" on the dogfood instance) — not a hardcoded "config/".
//
// An empty configRoot means the instance backs config with a dedicated repo
// whose whole tree is config: any in-repo path is allowed, but a path escaping
// the repo (absolute, or via "..") is still refused. On a same-repo instance —
// where platform code and config share one repo, as on the dogfood instance —
// the caller MUST pass a non-empty root, otherwise this only guards repo-escape,
// not platform paths.
//
// An empty changed set is not an error: a cycle that proposes no change simply
// has nothing to confine.
func Confine(configRoot string, changed []string) error {
	root := normalizeConfigRoot(configRoot)
	if root == "" && !wholeRepoConfigRoot(configRoot) {
		return fmt.Errorf("%w: invalid configured root %q", ErrOutsideConfigRoot, configRoot)
	}
	for _, p := range changed {
		if err := pathWithinRoot(root, p); err != nil {
			return err
		}
	}
	return nil
}

// ErrNoDocsRoots marks a docs-roots confinement asked for with an empty root
// set. Unlike Confine's empty configRoot (which means "the whole repo is
// config"), an empty docs-roots list can only be a misconfiguration: a
// docs-updater run that declares no roots has no legitimate place to write, so
// this fails closed rather than silently allowing the whole tree.
var ErrNoDocsRoots = errors.New("configboundary: no docs roots declared")

// ConfineToAny returns nil only when every path in changed is contained within
// at least one of roots. It is the multi-root analog of Confine, for the
// docs-updater write boundary (#1016): a docs-updater run may write to any of
// its several declared documentation roots (e.g. "docs", "README.md"), but a
// path outside all of them — code, CI, credentials — is refused, exactly as
// Confine refuses a path outside the single config root.
//
// roots must be non-empty; each root that normalizes to "" (empty, absolute, or
// escaping — a bogus root) is dropped rather than widened to whole-repo, so a
// malformed root can never silently open the boundary. If every root is bogus,
// the effective set is empty and every change is refused (ErrNoDocsRoots),
// failing closed. An empty changed set is not an error.
func ConfineToAny(roots []string, changed []string) error {
	normalized := make([]string, 0, len(roots))
	for _, r := range roots {
		if n := normalizeConfigRoot(r); n != "" {
			normalized = append(normalized, n)
		}
	}
	if len(normalized) == 0 {
		return ErrNoDocsRoots
	}
	for _, p := range changed {
		if err := pathWithinAnyRoot(normalized, p); err != nil {
			return err
		}
	}
	return nil
}

// pathWithinAnyRoot returns nil when p is inside any of roots (all normalized,
// none empty). It reports the last containment error when p is outside every
// root — enough to name the offending path in the failure.
func pathWithinAnyRoot(roots []string, p string) error {
	var last error
	for _, root := range roots {
		if err := pathWithinRoot(root, p); err == nil {
			return nil
		} else {
			last = err
		}
	}
	if last == nil {
		return fmt.Errorf("%w: %q", ErrOutsideConfigRoot, p)
	}
	return last
}

// normalizeConfigRoot cleans a configured root to a comparable, repo-relative
// form. Empty / slash-only normalizes to "" (whole-repo). Invalid roots also
// normalize to "", but Confine distinguishes them with wholeRepoConfigRoot and
// fails closed rather than widening the boundary.
func normalizeConfigRoot(configRoot string) string {
	spaced := strings.TrimSpace(configRoot)
	if rootedOrVolumeBound(spaced) {
		return ""
	}
	trimmed := strings.Trim(spaced, "/")
	if trimmed == "" {
		return ""
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ""
	}
	return clean
}

func wholeRepoConfigRoot(configRoot string) bool {
	spaced := strings.TrimSpace(configRoot)
	return strings.Trim(spaced, "/") == ""
}

// pathWithinRoot returns nil only when the repo-relative path p is inside root.
// root is assumed normalized ("" = whole repo). p is refused when empty,
// absolute, or resolving outside root via "..".
func pathWithinRoot(root, p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("%w: empty path", ErrOutsideConfigRoot)
	}
	if rootedOrVolumeBound(p) {
		return fmt.Errorf("%w: %q is absolute or volume-bound", ErrOutsideConfigRoot, p)
	}
	clean := filepath.Clean(p)
	// Reject escape above the repo regardless of root.
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q escapes the repository", ErrOutsideConfigRoot, p)
	}
	if root == "" {
		return nil
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q is outside config root %q", ErrOutsideConfigRoot, p, root)
	}
	return nil
}

func rootedOrVolumeBound(path string) bool {
	return filepath.IsAbs(path) ||
		filepath.VolumeName(path) != "" ||
		strings.HasPrefix(path, "/") ||
		strings.HasPrefix(path, `\`) ||
		(len(path) >= 2 && path[1] == ':' &&
			((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')))
}
