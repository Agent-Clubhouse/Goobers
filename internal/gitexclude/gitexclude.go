// Package gitexclude registers harness-owned paths in a repository's local
// exclude list (`info/exclude`), so git never reports them as untracked.
//
// Excluding is preferred over every downstream "is this path worthless?"
// predicate because git's own semantics carry the safety constraint for free:
// an exclude pattern NEVER applies to a tracked path. A repository that
// legitimately commits a file of the same name keeps it visible, keeps it
// diffable, and keeps it captured — while the harness's own throwaway copy of
// that name in a managed workspace stays invisible to `git status`,
// `git ls-files --others --exclude-standard`, `git add -A`, and therefore to
// recovery snapshot capture (#5119).
//
// `git rev-parse --git-path info/exclude` resolves to the repository's COMMON
// git directory: for a linked worktree it is the mirror's file, shared by every
// worktree branched from it, not a per-worktree file (MEASURED). Patterns are
// therefore written root-anchored so they can only ever match at a worktree's
// own top level and never a same-named path nested somewhere inside a checkout
// of the same mirror.
package gitexclude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Pattern is one exclude line plus any legacy spellings that already satisfy
// it, so a repository written by an older version is not given a second,
// equivalent line on every pass.
type Pattern struct {
	Line    string
	Aliases []string
}

// Anchored builds a root-anchored Pattern for a workspace-relative path,
// escaping the glob metacharacters git would otherwise interpret. It returns
// false for a path that is empty, absolute, or that climbs out of the
// workspace — none of which names a file the harness owns at that root.
func Anchored(rel string) (Pattern, bool) {
	slashed := strings.TrimSpace(filepath.ToSlash(rel))
	if slashed == "" || strings.HasPrefix(slashed, "/") || filepath.IsAbs(rel) {
		return Pattern{}, false
	}
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(slashed)), "./")
	if clean == "" || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return Pattern{}, false
	}
	return Pattern{Line: "/" + escapeGlob(clean)}, true
}

// escapeGlob neutralizes the fnmatch metacharacters git honours in an exclude
// line, so a literal path containing one of them matches only itself.
func escapeGlob(path string) string {
	var b strings.Builder
	b.Grow(len(path) + 4)
	for _, r := range path {
		switch r {
		case '*', '?', '[', ']', '\\', '!', '#', ' ':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Ensure appends every pattern the repository at dir does not already carry to
// its info/exclude, and is a no-op when all of them are present. The write is a
// single append, so existing content — including an operator's own patterns —
// is preserved by construction, and a concurrent caller can at worst duplicate
// a line rather than truncate the file.
func Ensure(ctx context.Context, dir string, patterns ...Pattern) error {
	if len(patterns) == 0 {
		return nil
	}
	excludePath, err := excludeFilePath(ctx, dir)
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("gitexclude: read %s: %w", excludePath, err)
	}
	missing := missingPatterns(string(existing), patterns)
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("gitexclude: create %s: %w", filepath.Dir(excludePath), err)
	}
	return appendLines(excludePath, len(existing) > 0 && existing[len(existing)-1] != '\n', missing)
}

// excludeFilePath resolves dir's info/exclude. The path git reports is relative
// to dir unless git itself made it absolute, so both shapes are handled.
func excludeFilePath(ctx context.Context, dir string) (string, error) {
	// safe.bareRepository=all: a host configured with `explicit` refuses to
	// operate on a bare repository discovered from its path, which is exactly
	// how a managed mirror is addressed. The flag carries the highest config
	// precedence and is behavior-neutral everywhere else; hooks are pinned off
	// for the same reason the worktree layer pins them on every managed git
	// call. This command only reads a path.
	cmd := exec.CommandContext(ctx, "git",
		"-c", "safe.bareRepository=all",
		"-c", "core.hooksPath="+os.DevNull,
		"rev-parse", "--git-path", "info/exclude")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gitexclude: resolve info/exclude in %s: %w", dir, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("gitexclude: resolve info/exclude in %s: git reported an empty path", dir)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return path, nil
}

// missingPatterns reports the lines of patterns that content does not already
// carry, counting a pattern's declared aliases as satisfying it.
func missingPatterns(content string, patterns []Pattern) []string {
	present := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		present[strings.TrimSpace(line)] = true
	}
	missing := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern.Line == "" || present[pattern.Line] || satisfiedByAlias(present, pattern) {
			continue
		}
		missing = append(missing, pattern.Line)
	}
	return missing
}

func satisfiedByAlias(present map[string]bool, pattern Pattern) bool {
	for _, alias := range pattern.Aliases {
		if present[alias] {
			return true
		}
	}
	return false
}

func appendLines(path string, needsLeadingNewline bool, lines []string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("gitexclude: open %s: %w", path, err)
	}
	var buf strings.Builder
	if needsLeadingNewline {
		buf.WriteString("\n")
	}
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	if _, err := file.WriteString(buf.String()); err != nil {
		_ = file.Close()
		return fmt.Errorf("gitexclude: write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("gitexclude: close %s: %w", path, err)
	}
	return nil
}
