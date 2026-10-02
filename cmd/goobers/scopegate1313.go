package main

import (
	"context"
	"fmt"
	"io"

	"github.com/goobers/goobers/providers"
)

// scopeGateLabel marks a PR that cannot be merged autonomously because its diff
// meets or exceeds the #1313 size threshold. Review still runs so findings can
// drive remediation; the carried scopeGateParked output stops the merge path.
const scopeGateLabel = "goobers:scope-gate"

// scopeGateAckLabel lets an operator explicitly release a parked PR for
// autonomous merge despite its size — the "operator ack" half of #1313's acceptance
// criteria. Persistent, not one-shot: it stays in effect until a human
// removes it, exactly like every other durable override label in this
// codebase (goobers:merge-demoted, goobers:merge-escalated).
const scopeGateAckLabel = "goobers:scope-gate-ack"

// scopeGateAckLabelColor/Description define the ack label so the park path can
// create it on demand. The gate names this label as the operator's only escape
// hatch that does not require rewriting the PR, so the label has to actually
// exist in the repository for the instruction to mean anything — and nothing
// created it. It was absent from Agent-Clubhouse/Goobers entirely, which turned
// a two-exit gate into a one-exit gate: PR #1748 sat parked ~16 hours because
// the only reachable exit was shrinking a cohesive 5,313-line subsystem, which
// no remediation cycle can do (#1801).
const (
	scopeGateAckLabelColor       = "D4C5F9"
	scopeGateAckLabelDescription = "Operator acknowledgement that an oversized PR may merge past the #1313 scope gate"
)

// ensureScopeGateAckLabel makes the ack label exist before the park comment
// tells an operator to apply it. Idempotent (EnsureWorkItemLabels skips labels
// that already exist) and best-effort by the same contract as the rest of this
// file: failing to pre-create a label must never itself block or unblock a
// merge, so the caller only warns. Asserting the label at the moment the
// instruction is issued is what keeps the two in sync — a constant that names
// repository state nothing validates is the same failure mode as the
// ruleset-pinned required-ci job name.
type scopeGateProvider interface {
	scopeDriftProvider
	EnsureWorkItemLabels(context.Context, providers.RepositoryRef, []providers.WorkItemLabel) (providers.EnsureWorkItemLabelsResult, error)
}

func ensureScopeGateAckLabel(ctx context.Context, provider scopeGateProvider, repo providers.RepositoryRef) error {
	_, err := provider.EnsureWorkItemLabels(ctx, repo, []providers.WorkItemLabel{{
		Name:        scopeGateAckLabel,
		Color:       scopeGateAckLabelColor,
		Description: scopeGateAckLabelDescription,
	}})
	if err != nil {
		return fmt.Errorf("ensure %s exists: %w", scopeGateAckLabel, err)
	}
	return nil
}

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
//   - Shrunk back under both thresholds but still labeled: clear the label +
//     a release comment.
//   - Released by an operator ack while still labeled: leave the label in
//     place. pr-select's needs-remediation bypass requires BOTH scope-gate
//     labels, so clearing here would strand the PR if the run died before a
//     replacement verdict published (#1869). apply-verdict clears it via
//     releaseAcknowledgedScopeGate once that verdict is durable.
//   - Otherwise: no-op (idempotent — never re-comments while parked, never
//     comments on a PR that was never parked).
//
// Best-effort by contract, same as flagScopeDrift: the caller treats any
// error as a warning, since a labeling hiccup must never itself block or
// unblock a merge.
func reconcileScopeGate(ctx context.Context, provider scopeDriftProvider, repo providers.RepositoryRef, prNumber int, prLabels []string, changedFiles, changedLines, filesThreshold, linesThreshold int) (parked bool, changed bool, err error) {
	overFiles := filesThreshold > 0 && changedFiles >= filesThreshold
	overLines := linesThreshold > 0 && changedLines >= linesThreshold
	acked := hasAnyLabel(prLabels, []string{scopeGateAckLabel})
	parked = (overFiles || overLines) && !acked
	labeled := hasAnyLabel(prLabels, []string{scopeGateLabel})

	switch {
	case parked && !labeled:
		// Create the ack label before naming it, so the escape hatch the comment
		// below advertises is actually reachable (#1801). Deliberately non-fatal:
		// parking is the safe direction, so a labels-API hiccup must not stop the
		// gate from blocking. Worst case the operator has to create the label by
		// hand — the situation before this change, not a worse one.
		if labels, ok := provider.(scopeGateProvider); ok {
			_ = ensureScopeGateAckLabel(ctx, labels, repo)
		}
		comment := fmt.Sprintf(
			"🚧 **Scope gate** (#1313): this pull request %s — at or past the configured threshold for autonomous "+
				"merge. Unlike the advisory `goobers:scope-drift` flag (#1111), this label **blocks autonomous merge** "+
				"but does not suppress review: fresh findings can still drive remediation without allowing a mega-merge "+
				"to land (#1068, #1186). It clears automatically once the diff shrinks back under both thresholds on a "+
				"later run, or an operator can add `%s` to acknowledge the size and release the merge gate.",
			scopeGateSizeDescription(changedFiles, changedLines, filesThreshold, linesThreshold), scopeGateAckLabel)
		if _, uerr := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: fmt.Sprintf("%d", prNumber), AddLabels: []string{scopeGateLabel}, Comment: comment,
		}); uerr != nil {
			return parked, false, fmt.Errorf("apply %s to pr #%d: %w", scopeGateLabel, prNumber, uerr)
		}
		return parked, true, nil
	case !parked && labeled && !acked:
		if err := clearScopeGate(ctx, provider, repo, prNumber, "the diff shrunk back under both thresholds"); err != nil {
			return parked, false, err
		}
		return parked, true, nil
	}
	return parked, false, nil
}

// releaseAcknowledgedScopeGate performs the ack-driven clear reconcileScopeGate
// defers (#1869). apply-verdict calls it only after the replacement verdict is
// published, so a run that dies earlier leaves both scope-gate labels on the
// PR and pr-select can still select it at an unchanged head/base. parked is
// the gate decision the verdict was published under: a PR parked at gather
// time (the ack arrived mid-run) keeps its label for the next cycle to
// re-evaluate. Best-effort like the rest of this file: a failed clear leaves
// the PR in the still-selectable both-labels state.
func releaseAcknowledgedScopeGate(ctx context.Context, provider scopeDriftProvider, repo providers.RepositoryRef, prNumber int, prLabels []string, parked bool) (bool, error) {
	if parked || !hasAnyLabel(prLabels, []string{scopeGateLabel}) || !hasAnyLabel(prLabels, []string{scopeGateAckLabel}) {
		return false, nil
	}
	if err := clearScopeGate(ctx, provider, repo, prNumber, "an operator added "+scopeGateAckLabel); err != nil {
		return false, err
	}
	return true, nil
}

func clearScopeGate(ctx context.Context, provider scopeDriftProvider, repo providers.RepositoryRef, prNumber int, reason string) error {
	comment := fmt.Sprintf("✅ **Scope gate cleared** (#1313): %s — autonomous merge is eligible again.", reason)
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: fmt.Sprintf("%d", prNumber), RemoveLabels: []string{scopeGateLabel}, Comment: comment,
	}); err != nil {
		return fmt.Errorf("clear %s from pr #%d: %w", scopeGateLabel, prNumber, err)
	}
	return nil
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

// releaseAcknowledgedScopeGateAfterVerdict is apply-verdict's call site for
// releaseAcknowledgedScopeGate: it reads the gate decision the verdict was
// published under and reports the outcome without failing the stage.
func releaseAcknowledgedScopeGateAfterVerdict(ctx context.Context, provider scopeDriftProvider, repo providers.RepositoryRef, prNumber int, prLabels []string, stdout, stderr io.Writer) {
	parked := providerInput("scopeGateParked", "") == "true"
	released, err := releaseAcknowledgedScopeGate(ctx, provider, repo, prNumber, prLabels, parked)
	if err != nil {
		pf(stderr, "warning: scope gate: %v\n", err)
	} else if released {
		pf(stdout, "scope-gate: PR #%d released by %s — cleared %s\n", prNumber, scopeGateAckLabel, scopeGateLabel)
	}
}
