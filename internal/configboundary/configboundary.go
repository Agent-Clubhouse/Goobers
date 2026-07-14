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
	for _, p := range changed {
		if err := pathWithinRoot(root, p); err != nil {
			return err
		}
	}
	return nil
}

// normalizeConfigRoot cleans a configured root to a comparable, repo-relative
// form. Empty / slash-only normalizes to "" (whole-repo). A root that itself
// escapes (absolute, ".", or "..") also normalizes to "" so a bogus root can
// never be treated as a real subtree — callers get whole-repo containment, the
// safe floor, rather than a widened boundary.
func normalizeConfigRoot(configRoot string) string {
	spaced := strings.TrimSpace(configRoot)
	if filepath.IsAbs(spaced) {
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

// pathWithinRoot returns nil only when the repo-relative path p is inside root.
// root is assumed normalized ("" = whole repo). p is refused when empty,
// absolute, or resolving outside root via "..".
func pathWithinRoot(root, p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("%w: empty path", ErrOutsideConfigRoot)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%w: %q is absolute", ErrOutsideConfigRoot, p)
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
