package recovery

import "strings"

// Goobers' own stage machinery writes its bookkeeping into the target
// repository's worktree: the mutation ledger a stage appends to, the claimed
// backlog item a selection stage resolves, and anything under the reserved
// .goobers directory. Snapshot capture selects paths with
// `ls-files --cached --others --exclude-standard`, which honours .gitignore,
// so these files are captured precisely because they are untracked and not
// ignored in the target repository.
//
// A sibling inflow change excludes them at capture, so a fresh capture whose
// only changes were these is discarded by the existing empty-diff guard
// instead of consuming a slot. This list exists for the captures taken BEFORE
// that change: an instance that is already wedged still holds them, and a
// snapshot whose diff touches nothing else protects no agent-authored work.
//
// Matching is deliberately narrow. The named files count only at the
// repository root, where the harness writes them, so a repository that
// legitimately tracks docs/mutations.jsonl never loses committed work.
const harnessArtifactDirectory = ".goobers"

// HarnessArtifactPaths returns the repository-root files Goobers' own stages
// write into a target worktree. The returned slice is a copy; callers must
// not treat it as a stable ordering.
func HarnessArtifactPaths() []string {
	return []string{"mutations.jsonl", "claimed-item.json", "claimed-items.json"}
}

// HarnessOwnedPath reports whether one repository-relative path, as Git emits
// it in a diff (always forward-slash separated), is a Goobers stage artifact
// rather than agent-authored content.
func HarnessOwnedPath(path string) bool {
	if path == "" {
		return false
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == harnessArtifactDirectory {
			return true
		}
	}
	if len(segments) != 1 {
		return false
	}
	for _, artifact := range HarnessArtifactPaths() {
		if segments[0] == artifact {
			return true
		}
	}
	return false
}

// BookkeepingOnlySnapshot reports whether a snapshot's touched paths are
// non-empty and consist entirely of Goobers stage artifacts. An empty list is
// NOT bookkeeping-only: a snapshot that touches nothing is the separate
// no-diff case, and an unavailable path listing must never be read as proof
// that there was nothing to keep.
func BookkeepingOnlySnapshot(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		if !HarnessOwnedPath(path) {
			return false
		}
	}
	return true
}

// HasNoDiff reports whether the captured snapshot matched its base exactly,
// so the retained entry holds no recoverable patch bytes at all.
func (r Record) HasNoDiff() bool {
	return r.PatchDigest == emptyPatchDigest
}
