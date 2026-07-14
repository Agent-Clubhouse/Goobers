package tutor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/providers"
)

// ErrOutsideConfigRoot marks a proposal that would write outside the instance's
// configured config root — the T4 (#104) write-boundary. It fails closed: a run
// that would breach the boundary is aborted before any branch, commit, or
// on-disk config read happens, so platform paths are structurally unreachable
// through the Tutor workflow.
//
// This is the path-scoped half of the boundary. The *structural* half — a
// credential that cannot push platform changes even if this check were bypassed
// — is deferred to #35; see docs/guides/tutor-write-boundary.md.
var ErrOutsideConfigRoot = errors.New("tutor: proposal path escapes configured config root")

// enforceConfigBoundary rejects a proposal unless every file it would write is
// contained within configRoot. configRoot is the instance-configured, repo-
// relative config root (e.g. "selfhost" on the dogfood instance) — not a
// hardcoded "config/".
//
// An empty configRoot means the instance backs config with a dedicated repo
// whose whole tree is config (the separate-config-repo case): any in-repo path
// is allowed, but a path escaping the repo (absolute, or via "..") is still
// refused. On a *same-repo* instance — where platform code and config live in
// one repo, as on the dogfood instance — the operator MUST configure a non-empty
// root, otherwise this boundary only guards repo-escape, not platform paths.
// See docs/guides/tutor-write-boundary.md.
//
// The containment test is lexical (the proposal's files do not exist on disk
// yet), mirroring the pattern in api/v1alpha1.ResolveContainedPath (#120).
func enforceConfigBoundary(configRoot string, files []providers.CommitFile) error {
	root := normalizeConfigRoot(configRoot)
	for _, f := range files {
		if err := pathWithinRoot(root, f.Path); err != nil {
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
	// An absolute root is a misconfiguration (roots are repo-relative); collapse
	// to "" before trimming slashes so it can't masquerade as a subtree.
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
