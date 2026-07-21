package main

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/providers"
)

// scopeDriftLabel flags a PR whose diff has grown far past an ordinary change's
// size (#1111) — the mega-merge shape that findings 009/010 showed drags in
// sibling scope and ships silent defects a blind rebase-and-merge would miss
// (e.g. #1068: 73 files, +4781/-329, declared as a single-issue change). The
// label is advisory: it never blocks the merge (#581's "delete failures are
// visible but do not rewrite the merge result" posture), it only surfaces the
// drift so a human/reviewer notices before a large merge lands unremarked.
const scopeDriftLabel = "goobers:scope-drift"

// defaultScopeDriftThreshold is the changed-file count above which a PR is
// flagged. Chosen well above ordinary goober PRs (a fix touches a handful of
// files; even a broad feature rarely 30+) so it does not false-positive, while
// still catching a genuine mega-merge. Overridable via the scopeDriftThreshold
// input; 0 disables the guard entirely.
const defaultScopeDriftThreshold = 50

// flagScopeDrift keeps goobers:scope-drift in sync with the selected PR's
// changed-file count (#1111): it applies the label + a one-time explanatory
// comment when the PR first exceeds the threshold, and removes it if the PR has
// since shrunk back under (e.g. a rebase split it up). Idempotent — the label's
// presence gates the comment, so re-running each merge-review cycle never
// re-comments. Best-effort by contract: the caller treats any error as a
// warning and never fails the review stage on it, since this is a flag, not a
// gate.
func flagScopeDrift(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, prNumber int, prLabels []string, changedFiles, threshold int) (changed bool, err error) {
	if threshold <= 0 {
		return false, nil
	}
	over := changedFiles > threshold
	labeled := hasAnyLabel(prLabels, []string{scopeDriftLabel})
	switch {
	case over && !labeled:
		comment := fmt.Sprintf(
			"⚠️ **Scope drift** (#1111): this pull request changes **%d files**, well past the %d-file flag threshold for an ordinary change. Large diffs like this are where the mega-merge failure mode lives — a PR that absorbs sibling scope over many remediation cycles can carry far more than its declared issue calls for, and a blind rebase-and-merge on such a diff has shipped silent defects before (findings 009/010, e.g. #1068). This label is **advisory only** — it does not block the merge; it just asks a human to confirm the diff really matches the declared scope before it lands. It clears automatically if the PR shrinks back under the threshold.",
			changedFiles, threshold)
		if _, uerr := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: fmt.Sprintf("%d", prNumber), AddLabels: []string{scopeDriftLabel}, Comment: comment,
		}); uerr != nil {
			return false, fmt.Errorf("apply %s to pr #%d: %w", scopeDriftLabel, prNumber, uerr)
		}
		return true, nil
	case !over && labeled:
		if _, uerr := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: fmt.Sprintf("%d", prNumber), RemoveLabels: []string{scopeDriftLabel},
		}); uerr != nil {
			return false, fmt.Errorf("clear %s from pr #%d: %w", scopeDriftLabel, prNumber, uerr)
		}
		return true, nil
	}
	return false, nil
}
