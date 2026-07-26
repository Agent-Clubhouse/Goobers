package main

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/providers"
)

// scopeGateLabel marks a PR merge-review has PARKED before review because its
// diff meets or exceeds the #1313 size threshold — escalating #1111's
// advisory goobers:scope-drift flag (which never blocks) into an actual
// merge-review gate. Unlike scope-drift, a PR carrying this label does not
// reach the review gate at all this cycle: gather-sibling-context's
// scopeGateParked output routes merge-review's scope-gate branch straight to
// completion instead of review.
const scopeGateLabel = "goobers:scope-gate"

// scopeGateAckLabel lets an operator explicitly release a parked PR for
// review despite its size — the "operator ack" half of #1313's acceptance
// criteria. Persistent, not one-shot: it stays in effect until a human
// removes it, exactly like every other durable override label in this
// codebase (goobers:merge-demoted, goobers:merge-escalated).
const scopeGateAckLabel = "goobers:scope-gate-ack"

// defaultScopeGateFilesThreshold/defaultScopeGateLinesThreshold are the
// maintainer-ruled defaults (issue #1313): comfortably above an ordinary PR,
// comfortably below the observed bad cases (73-94 files in #1068/#1186).
// Either dimension tripping parks the PR — whichever trips first.
// Overridable via the scopeGateFilesThreshold/scopeGateLinesThreshold
// inputs; <= 0 disables that dimension.
const (
	defaultScopeGateFilesThreshold = 50
	defaultScopeGateLinesThreshold = 2000
)

// reconcileScopeGate keeps goobers:scope-gate in sync with whether the
// selected PR is currently over threshold (#1313). Stateless by design: parked
// status is recomputed fresh from live provider data every merge-review
// cycle, so there is no snapshot to go stale (PRL-061's "every park state
// must have a deterministic exit" — self-heal here needs no recorded state at
// all, only a live recompute) — the label just always reflects the current
// answer:
//
//   - Over threshold and not acked, not yet labeled: apply the label + an
//     explanatory comment. This is a BLOCKING park (unlike scope-drift's pure
//     advisory), so the comment says so explicitly.
//   - No longer parked (shrunk back under both thresholds, OR an operator
//     added the ack label) but still labeled: clear the label + a release
//     comment naming which of the two happened.
//   - Otherwise: no-op (idempotent — never re-comments while parked, never
//     comments on a PR that was never parked).
//
// Best-effort by contract, same as flagScopeDrift: the caller treats any
// error as a warning, since a labeling hiccup must never itself block or
// unblock a merge.
func reconcileScopeGate(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, prNumber int, prLabels []string, changedFiles, changedLines, filesThreshold, linesThreshold int) (parked bool, changed bool, err error) {
	overFiles := filesThreshold > 0 && changedFiles >= filesThreshold
	overLines := linesThreshold > 0 && changedLines >= linesThreshold
	acked := hasAnyLabel(prLabels, []string{scopeGateAckLabel})
	parked = (overFiles || overLines) && !acked
	labeled := hasAnyLabel(prLabels, []string{scopeGateLabel})

	switch {
	case parked && !labeled:
		comment := fmt.Sprintf(
			"🚧 **Scope gate** (#1313): this pull request %s — at or past the configured threshold for autonomous "+
				"merge. Unlike the advisory `goobers:scope-drift` flag (#1111), this label **blocks** merge-review "+
				"from proceeding to review this cycle: diffs this large are where mega-merge failures have shipped "+
				"before despite review (#1068, #1186). It clears automatically once the diff shrinks back under both "+
				"thresholds on a later run, or an operator can add `%s` to release it for review despite the size.",
			scopeGateSizeDescription(changedFiles, changedLines, filesThreshold, linesThreshold), scopeGateAckLabel)
		if _, uerr := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: fmt.Sprintf("%d", prNumber), AddLabels: []string{scopeGateLabel}, Comment: comment,
		}); uerr != nil {
			return parked, false, fmt.Errorf("apply %s to pr #%d: %w", scopeGateLabel, prNumber, uerr)
		}
		return parked, true, nil
	case !parked && labeled:
		reason := "an operator added " + scopeGateAckLabel
		if !acked {
			reason = "the diff shrunk back under both thresholds"
		}
		comment := fmt.Sprintf("✅ **Scope gate cleared** (#1313): %s — proceeding to review normally this cycle.", reason)
		if _, uerr := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: fmt.Sprintf("%d", prNumber), RemoveLabels: []string{scopeGateLabel}, Comment: comment,
		}); uerr != nil {
			return parked, false, fmt.Errorf("clear %s from pr #%d: %w", scopeGateLabel, prNumber, uerr)
		}
		return parked, true, nil
	}
	return parked, false, nil
}

// scopeGateSizeDescription names whichever dimension(s) actually tripped, so
// the parking comment is specific rather than generically "too big".
func scopeGateSizeDescription(changedFiles, changedLines, filesThreshold, linesThreshold int) string {
	overFiles := filesThreshold > 0 && changedFiles >= filesThreshold
	overLines := linesThreshold > 0 && changedLines >= linesThreshold
	switch {
	case overFiles && overLines:
		return fmt.Sprintf("changes **%d files** and **%d lines** (thresholds: %d files / %d lines)",
			changedFiles, changedLines, filesThreshold, linesThreshold)
	case overFiles:
		return fmt.Sprintf("changes **%d files** (threshold: %d)", changedFiles, filesThreshold)
	default:
		return fmt.Sprintf("changes **%d lines** (threshold: %d)", changedLines, linesThreshold)
	}
}
