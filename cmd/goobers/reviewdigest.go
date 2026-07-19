package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// verdictSchemaVersion is bumped whenever apiv1.Verdict's shape or
// verdictJSONComment's payload encoding changes in a way that would make an
// OLD cached verdict misleading if silently reused (issue #523's ruling: "a
// verdict-schema version bump" is one of the four things that must force a
// fresh review). Folded into reviewDigest so a bump invalidates every
// standing cache entry without touching any PR's state.
const verdictSchemaVersion = 2

// hunkHeaderPattern matches a unified-diff hunk header's line-number
// portion (e.g. "@@ -12,7 +12,7 @@ func Foo() {") — the part that shifts
// under a disjoint rebase (an unrelated earlier change in the same file
// moves everything after it) even though the actual added/removed lines
// are byte-identical. patchIdentity strips it before hashing — the same
// normalization `git patch-id --stable` performs on a real diff — without
// needing a git subprocess, a checkout, or any local clone: issue #718
// explicitly allows "(or hash of the diff)" as the re-keying mechanism,
// and GitHub's own per-file patch text (ChangedFile.Patch) is already
// exactly the unified-diff hunk body this needs.
var hunkHeaderPattern = regexp.MustCompile(`(?m)^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@`)

// patchIdentity computes a rebase-invariant content identity for a PR's
// full changeset (issue #718, replacing computeReviewDigest's old raw
// head-SHA component): sorted by path so gather order is never
// significant, each file contributes its path, status, and hunk text with
// line-numbers normalized out — so a clean rebase (head SHA changes, patch
// content doesn't) produces the SAME identity, while any real content
// change produces a different one. A file GitHub reports no patch for
// (binary, or over its per-file diff-size cutoff) still contributes its
// path/status/line-counts, so a change to it is still detected even though
// its content can't be normalized.
func patchIdentity(files []providers.ChangedFile) string {
	sorted := append([]providers.ChangedFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		if f.Patch != "" {
			normalized := hunkHeaderPattern.ReplaceAllString(f.Patch, "@@")
			_, _ = fmt.Fprintf(h, "file:%s:%s:%s\n", f.Path, f.Status, normalized)
		} else {
			_, _ = fmt.Fprintf(h, "file:%s:%s:+%d-%d\n", f.Path, f.Status, f.Additions, f.Deletions)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// sortedFileSetDigest hashes a set of file paths, order-independent —
// issue #718's replacement for keying a sibling on its raw head SHA (a
// sibling force-push that doesn't change WHICH files it touches shouldn't
// invalidate this PR's cached verdict) and for keying "what base touched
// since this PR's merge-base" (only relevant if it overlaps this PR's own
// files — see computeReviewDigest's baseIntersectionDigest parameter).
func sortedFileSetDigest(paths []string) string {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, p := range sorted {
		_, _ = fmt.Fprintf(h, "%s\n", p)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// computeReviewDigest is merge-review's cross-run cache key (issue #523's
// maintainer ruling, re-keyed by issue #718 to actually hit): a content
// hash of every input the holistic reviewer's verdict actually depends on.
//
//   - selectedPatchIdentity: the selected PR's own patch content (patchIdentity),
//     not its raw head SHA — invariant to a clean rebase.
//   - baseIntersectionDigest: sortedFileSetDigest of (files base has touched
//     since this PR's own merge-base) ∩ (this PR's own files) — invariant to
//     a base advance that never touches anything this PR cares about; the
//     caller (gather-sibling-context) computes the intersection, this
//     function just hashes the result.
//   - siblings: each contributes (number, sortedFileSetDigest(its files)),
//     not its raw head SHA — invariant to a sibling force-push that
//     doesn't change WHICH files it touches.
//
// Two gathers with an identical digest saw byte-identical review inputs
// under this (now content-based, not identity-based) equivalence, so a
// verdict computed for one is exactly as valid for the other. Siblings are
// sorted by number first so gather order (a map/slice iteration artifact,
// never semantically meaningful) can never perturb the digest.
func computeReviewDigest(selectedPatchIdentity, baseIntersectionDigest string, siblings []siblingPR) string {
	sorted := append([]siblingPR(nil), siblings...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	h := sha256.New()
	// hash.Hash.Write never returns an error (per its doc comment) — the
	// error return exists only to satisfy io.Writer.
	_, _ = fmt.Fprintf(h, "v%d\npatch:%s\nbase:%s\n", verdictSchemaVersion, selectedPatchIdentity, baseIntersectionDigest)
	for _, s := range sorted {
		_, _ = fmt.Fprintf(h, "sibling:%d:%s\n", s.Number, sortedFileSetDigest(s.Files))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// computeSelectedPatchContext gathers the two git-content-derived digest
// components issue #718 needs for the selected PR:
//
//   - patchID: patchIdentity of the PR's own files — GitHub's
//     compare(base...head) is exactly the same diff pulls/{n}/files
//     reports, fetched directly here (rather than via PullRequestFiles) so
//     the SAME call also returns the merge-base SHA the second component
//     needs.
//   - baseIntersectionDigest: sortedFileSetDigest of (files base has
//     touched since this PR's own merge-base) ∩ (this PR's own files) — a
//     second compare, from that merge-base to base's current tip,
//     intersected against the first compare's file set.
//
// A base that hasn't moved past the PR's merge-base yet (the common case
// for a freshly opened PR) short-circuits to an empty intersection without
// a second API call.
func computeSelectedPatchContext(ctx context.Context, provider providers.RepoProvider, repo providers.RepositoryRef, selectedBaseSHA, selectedHeadSHA string) (patchID, baseIntersectionDigest string, err error) {
	selfCompare, err := provider.CompareCommits(ctx, repo, selectedBaseSHA, selectedHeadSHA)
	if err != nil {
		return "", "", fmt.Errorf("compare PR's own base...head: %w", err)
	}
	patchID = patchIdentity(selfCompare.Files)

	if selfCompare.MergeBaseSHA == "" || selfCompare.MergeBaseSHA == selectedBaseSHA {
		return patchID, sortedFileSetDigest(nil), nil
	}
	baseCompare, err := provider.CompareCommits(ctx, repo, selfCompare.MergeBaseSHA, selectedBaseSHA)
	if err != nil {
		return "", "", fmt.Errorf("compare base's merge-base...current base: %w", err)
	}

	ownFiles := make(map[string]struct{}, len(selfCompare.Files))
	for _, f := range selfCompare.Files {
		ownFiles[f.Path] = struct{}{}
	}
	var intersecting []string
	for _, f := range baseCompare.Files {
		if _, ok := ownFiles[f.Path]; ok {
			intersecting = append(intersecting, f.Path)
		}
	}
	return patchID, sortedFileSetDigest(intersecting), nil
}

type authenticatedBacklogProvider interface {
	providers.BacklogProvider
	AuthenticatedLogin(context.Context) (string, error)
}

// findCachedVerdict looks up the most recently posted trusted verdict comment on
// selectedNumber (the same verdictJSONComment payload apply-verdict already
// posts, parsed by parseVerdictComment — gather-pr-context's exact
// mechanism for reading a merge-review verdict back from a DIFFERENT run's
// comments, reused here rather than duplicated) and returns it only when
// its recorded Digest matches wantDigest. A prior verdict with no Digest
// (posted before #523, or by a schema version this instance no longer
// trusts) or a non-matching Digest is not a cache hit — the caller falls
// through to a real review, exactly as if no comment existed at all.
func findCachedVerdict(ctx context.Context, provider authenticatedBacklogProvider, repo providers.RepositoryRef, selectedNumber int, wantDigest string) (*apiv1.Verdict, error) {
	author, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve merge-review verdict author: %w", err)
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(selectedNumber))
	if err != nil {
		return nil, fmt.Errorf("list comments on PR #%d: %w", selectedNumber, err)
	}
	for i := len(comments) - 1; i >= 0; i-- {
		v, ok := parseTrustedVerdictComment(comments[i].Author, comments[i].Body, author)
		if !ok {
			continue
		}
		if v.Digest != "" && v.Digest == wantDigest {
			return &v, nil
		}
		// The latest verdict comment is the only one that could possibly
		// still be valid (an older one is superseded regardless of its own
		// digest) — stop at the first one found, matching or not.
		return nil, nil
	}
	return nil, nil
}
