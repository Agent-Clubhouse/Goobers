package main

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

// Contested-file dispatch awareness (#1085).
//
// `implementation`'s backlog selection (backlogquery.go) claims new work with
// no awareness of which files are already touched by currently-open PRs. When
// several concurrently-dispatched runs land in the same hot files,
// merge-review's holistic cross-PR overlap check correctly refuses to merge
// any of them until the cluster is sequenced — but nothing upstream stops new
// overlapping work being created faster than the cluster can drain, so the jam
// is self-reinforcing.
//
// This gives selection a soft signal: an eligible issue whose *referenced*
// files (parsed from its title/body — the only pre-implementation signal we
// have) are already contested by contestedFileMinPRs+ open PRs is
// deprioritized behind the disjoint candidates for this cycle, rather than fed
// into the cluster. It is deliberately imperfect and non-blocking:
//
//   - It never drops a candidate. When every eligible issue is contested (or
//     none references a recognizable file) the order is unchanged and FIFO
//     claiming proceeds exactly as before — no starvation.
//   - It is a stable partition (clean-then-contested), so FIFO order is
//     preserved within each group.
//   - The provider fetch it needs is best-effort at the call site: any error
//     falls back to plain FIFO. Dispatch must never stall on this signal.
//
// It is distinct from the open-PR cap (#353/#986) and the contested-cluster
// *election* policy (#1028/#1029): those bound how many PRs are open and pick
// which already-open PR wins a contested cluster. This is the only place that
// keeps `implementation`'s own dispatch from actively *growing* the cluster.

// openPRTouch is one open PR's number and the set of file paths it changes —
// the ground truth backlog selection compares a candidate issue's referenced
// files against to decide whether it lands in an already-contested surface.
type openPRTouch struct {
	number int
	files  []string
}

// sourceFileRefPattern matches file-path-like tokens ending in a source-file
// extension. It is intentionally generous on the path portion (bare basename,
// partial path, or full repo path all match) because the real filter is the
// intersection with actually-contested PR files below: an over-broad match
// that happens to coincide with a hot file is, by definition, relevant, while
// an unrecognized reference simply isn't deprioritized (a miss, never a false
// block). The extension whitelist keeps version strings ("v1.2.3") and prose
// ("e.g.") out.
var sourceFileRefPattern = regexp.MustCompile(`[A-Za-z0-9_./-]+\.(?:go|ya?ml|json|tsx?|jsx?|md|sh|toml)\b`)

// referencedFilePaths extracts the distinct file-path tokens an issue's text
// mentions, in first-seen order. A leading "./" is stripped so a reference
// written "./internal/foo.go" matches a PR path "internal/foo.go".
func referencedFilePaths(text string) []string {
	matches := sourceFileRefPattern.FindAllString(text, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		ref := strings.TrimPrefix(m, "./")
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// fileRefMatchesPath reports whether a referenced token names the PR file at
// prPath. A full path matches exactly; a bare basename or partial path matches
// as a path suffix ("run.go" matches "internal/runner/run.go", "runner/run.go"
// matches it too). Requiring a "/"-aligned suffix — not a substring — keeps
// "gate.go" from matching "delegate.go".
func fileRefMatchesPath(prPath, ref string) bool {
	return prPath == ref || strings.HasSuffix(prPath, "/"+ref)
}

// distinctPRsTouchingRefs counts how many of touches change at least one file
// matching any of refs. Zero when refs is empty (an issue that names no
// recognizable file is never treated as contested).
func distinctPRsTouchingRefs(refs []string, touches []openPRTouch) int {
	if len(refs) == 0 {
		return 0
	}
	count := 0
	for _, t := range touches {
		if prTouchesAnyRef(t.files, refs) {
			count++
		}
	}
	return count
}

func prTouchesAnyRef(files, refs []string) bool {
	for _, f := range files {
		for _, r := range refs {
			if fileRefMatchesPath(f, r) {
				return true
			}
		}
	}
	return false
}

// partitionByContention returns eligible reordered so that every issue whose
// referenced files are contested by minPRs+ of touches is moved after the
// disjoint ("clean") issues, FIFO order preserved within each group. The
// second return is the IDs that were deprioritized, in their original order,
// for logging. minPRs < 1 is clamped to 1.
func partitionByContention(eligible []providers.WorkItem, touches []openPRTouch, minPRs int) ([]providers.WorkItem, []string) {
	if minPRs < 1 {
		minPRs = 1
	}
	clean := make([]providers.WorkItem, 0, len(eligible))
	contested := make([]providers.WorkItem, 0)
	var contestedIDs []string
	for _, it := range eligible {
		refs := referencedFilePaths(it.Title + "\n" + it.Body)
		if distinctPRsTouchingRefs(refs, touches) >= minPRs {
			contested = append(contested, it)
			contestedIDs = append(contestedIDs, it.ID)
			continue
		}
		clean = append(clean, it)
	}
	return append(clean, contested...), contestedIDs
}

// openPRTouches lists every open goober-authored PR and the files it changes,
// for contention scoring. It mirrors the open-PR backstop's HeadPrefix filter
// (backlogquery.go) and reuses PullRequestFiles (the same per-PR file view
// merge-review's sibling context uses). SkipCheckState avoids the two extra
// check-state requests per PR that this caller has no use for. Any error is
// returned to the caller, which treats it as best-effort (falls back to FIFO).
func openPRTouches(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, base string) ([]openPRTouch, error) {
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: providerBranchNamespace(), SkipCheckState: true,
	})
	if err != nil {
		return nil, err
	}
	touches := make([]openPRTouch, 0, len(prs))
	for _, pr := range prs {
		files, err := provider.PullRequestFiles(ctx, repo, strconv.Itoa(pr.Number))
		if err != nil {
			return nil, err
		}
		paths := make([]string, 0, len(files))
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		touches = append(touches, openPRTouch{number: pr.Number, files: paths})
	}
	return touches, nil
}
