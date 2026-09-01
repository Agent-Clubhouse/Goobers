package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

const defaultStaleAfter = 90 * 24 * time.Hour

var trackingChecklistIssuePattern = regexp.MustCompile(`(?m)^\s*[-*+]\s+\[[ xX]\].*?#([1-9][0-9]*)\b`)

var backlogReconcileReservationSequence atomic.Uint64

const (
	trackingOpenReadyReason = "removed `goobers:ready` because a tracking issue with open children is not directly implementable"
	trackingCompleteReason  = "removed `tracking` because the issue has no open provider or checklist children"
	trackingAutoCloseReason = "closed the opted-in tracking issue because it has no open provider or checklist children"
)

type backlogMetadataCorrection struct {
	addLabels           []string
	removeLabels        []string
	reasons             []string
	checkClaim          bool
	orphanedClaim       bool
	trackingComplete    bool
	closeTrackingParent bool
}

type inspectedBacklogItem struct {
	item       providers.WorkItem
	correction backlogMetadataCorrection
}

type backlogReconcileReservation struct {
	itemID   string
	gaggle   string
	provider string
	runID    string
}

func reconcileBacklogMetadata(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	trustLabel string,
	stalenessPolicy backlogStalenessPolicy,
	now func() time.Time,
) (int, error) {
	// Reap terminal and expired ledger leases before inspecting provider labels.
	// This makes the ledger's liveness decision available to the provider-marker
	// reconciliation below, so a dead claimant cannot keep its marker forever.
	// Through the seam (staleclaimrecovery.go): the daemon runs the sweep when
	// this stage is pod-dispatched and has no instance root of its own.
	if err := recoverStageClaims(l, now()); err != nil {
		return 0, fmt.Errorf("recover stale claims before metadata reconciliation: %w", err)
	}
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository:  repo,
		Labels:      []string{trustLabel},
		State:       "all",
		OldestFirst: true,
	})
	if err != nil {
		return 0, fmt.Errorf("list trusted backlog items: %w", err)
	}

	observedAt := now()
	blockedRecords, err := snapshotBlockedRecords(l)
	if err != nil {
		return 0, fmt.Errorf("snapshot learned block ledger: %w", err)
	}
	botLogin := ""
	reconciled := 0
	inspected := make([]inspectedBacklogItem, 0, len(items))
	for _, item := range items {
		// #1911: an item the ledger still records as blocked is inspected even
		// when it carries no reconciled marker — that is exactly the drifted
		// state where the label was cleared while the learned block holds.
		if !hasReconciledMetadataLabel(item) && len(recordedLedgerBlockers(blockedRecords, repo, item.ID)) == 0 {
			continue
		}
		current, err := provider.GetWorkItem(ctx, repo, item.ID)
		if err != nil {
			return reconciled, fmt.Errorf("refresh issue #%s: %w", item.ID, err)
		}
		correction, login, err := inspectBacklogMetadata(ctx, provider, repo, current, botLogin, observedAt, stalenessPolicy, blockedRecords)
		if err != nil {
			return reconciled, fmt.Errorf("inspect issue #%s: %w", item.ID, err)
		}
		botLogin = login
		if !correction.checkClaim && len(correction.removeLabels) == 0 && len(correction.addLabels) == 0 {
			continue
		}
		inspected = append(inspected, inspectedBacklogItem{item: current, correction: correction})
	}

	for _, inspectedItem := range inspected {
		current := inspectedItem.item
		correction := inspectedItem.correction
		if correction.trackingComplete {
			correction, err = revalidateCompletedTrackingItem(ctx, provider, repo, current.ID, correction)
			if err != nil {
				return reconciled, fmt.Errorf("revalidate tracking issue #%s: %w", current.ID, err)
			}
		}
		var reservation *backlogReconcileReservation
		if correction.checkClaim {
			var acquired bool
			reservation, acquired, err = reserveBacklogClaimReconciliation(l, repo, current.ID, now)
			if err != nil {
				return reconciled, fmt.Errorf("reserve claim reconciliation for issue #%s: %w", current.ID, err)
			}
			if acquired {
				correction.orphanedClaim = true
				correction.removeLabels = append(correction.removeLabels, providers.LabelClaimed)
				correction.reasons = append(correction.reasons,
					"removed `goobers:claimed` because no live claim-ledger lease backs it")
			} else {
				reservation = nil
			}
		}
		correction.removeLabels = uniqueSortedLabels(correction.removeLabels)
		correction.addLabels = uniqueSortedLabels(correction.addLabels)
		if len(correction.removeLabels) == 0 && len(correction.addLabels) == 0 && !correction.closeTrackingParent {
			continue
		}
		comment := reconciliationComment(correction.reasons)
		state := ""
		if correction.closeTrackingParent {
			state = "closed"
		}
		var correctionErr error
		if correction.orphanedClaim {
			// ReconcileOrphanedWorkItemClaim only removes labels, so any
			// close or label addition rides the edit that precedes it.
			if correction.closeTrackingParent || len(correction.addLabels) > 0 {
				_, correctionErr = provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
					Repository: repo,
					ID:         current.ID,
					AddLabels:  correction.addLabels,
					State:      state,
				})
			}
			if correctionErr == nil {
				_, correctionErr = provider.ReconcileOrphanedWorkItemClaim(
					ctx,
					repo,
					current.ID,
					correction.removeLabels,
					comment,
				)
			}
		} else {
			_, correctionErr = provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository:   repo,
				ID:           current.ID,
				AddLabels:    correction.addLabels,
				RemoveLabels: correction.removeLabels,
				State:        state,
				Comment:      comment,
			})
		}
		if correctionErr == nil {
			reconciled++
		}
		if reservation != nil {
			if releaseErr := releaseBacklogClaimReconciliation(l, *reservation); releaseErr != nil {
				correctionErr = errors.Join(correctionErr, fmt.Errorf("release claim-reconciliation reservation: %w", releaseErr))
			}
		}
		if correctionErr != nil {
			return reconciled, fmt.Errorf("reconcile issue #%s: %w", current.ID, correctionErr)
		}
	}
	return reconciled, nil
}

// backlogReconcileRunIDComponent is the fixed literal between the owning
// run's id and the pid/sequence suffix in a synthesized backlog-reconcile
// claim RunID (formatBacklogReconcileRunID). instance.Layout.FindRunDir
// rejects any run id containing "/" (runtime.go:139), so a claim reaper
// cannot look this synthesized id up directly — it must recover the OWNING
// run's id via parseBacklogReconcileRunID first and inspect that instead.
const backlogReconcileRunIDComponent = "backlog-reconcile"

// formatBacklogReconcileRunID synthesizes the claim RunID reserved for one
// backlog item's stale-metadata inspection: "<owner-run>/backlog-reconcile/
// <pid>/<seq>". This value is persisted directly into the claim ledger, so
// its shape must stay in sync with parseBacklogReconcileRunID below —
// existing ledger entries already use this exact format and must keep
// parsing after any change here.
func formatBacklogReconcileRunID(ownerRunID string, pid int, seq uint64) string {
	return fmt.Sprintf("%s/%s/%d/%d", ownerRunID, backlogReconcileRunIDComponent, pid, seq)
}

// parseBacklogReconcileRunID recovers the owning run id from a claim RunID
// produced by formatBacklogReconcileRunID. ok is false for any other shape,
// including a plain (non-reconcile) run id or a reconcile-shaped id whose
// pid/sequence suffix isn't purely numeric — callers must treat that as
// unparseable, not guess at a prefix.
func parseBacklogReconcileRunID(runID string) (ownerRunID string, ok bool) {
	before, after, found := strings.Cut(runID, "/"+backlogReconcileRunIDComponent+"/")
	if !found || before == "" {
		return "", false
	}
	pidPart, seqPart, ok := strings.Cut(after, "/")
	if !ok || !isDigitString(pidPart) || !isDigitString(seqPart) {
		return "", false
	}
	return before, true
}

func isDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func reserveBacklogClaimReconciliation(
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
	now func() time.Time,
) (*backlogReconcileReservation, bool, error) {
	gaggle := providerGaggle()
	ownerRunID := os.Getenv(executor.RunIDEnvVar)
	if ownerRunID == "" {
		ownerRunID = "standalone"
	}
	runID := formatBacklogReconcileRunID(ownerRunID, os.Getpid(), backlogReconcileReservationSequence.Add(1))
	ledger, err := openStageClaimLedger(l, localscheduler.WithLedgerClock(now))
	if err != nil {
		return nil, false, fmt.Errorf("open claim ledger: %w", err)
	}
	// Over the plane every call is contained to the bearer's run, so the
	// reservation is taken under the run's OWN id (finding 002 C1: "--reconcile's
	// reservation uses the run's own RunID so per-run containment holds").
	// The synthesized id's one job — refusing to reserve an item the owning
	// run itself holds, because a same-run claim would renew rather than
	// refuse — is kept by checking the run's holdings first.
	contained, onPlane := ledger.(claimsclient.Contained)
	if onPlane {
		runID = contained.ContainedRunID()
	}
	reservation := &backlogReconcileReservation{
		itemID:   itemID,
		gaggle:   gaggle,
		provider: string(repo.Provider),
		runID:    runID,
	}
	acquired := false
	err = ledger.Locked(claimContext(), claimLockOperationBacklogReconcile, func(tx claimsclient.Ledger) error {
		if onPlane {
			held, err := tx.ForRunAll(claimContext(), runID)
			if err != nil {
				return fmt.Errorf("read this run's claims: %w", err)
			}
			for _, entry := range held {
				if claimsclient.KeyForEntry(entry) == reservation.key() {
					return nil // the owning run holds it: not reservable, exactly as the synthesized id was refused
				}
			}
		}
		var err error
		acquired, _, err = tx.ClaimScoped(claimContext(), reservation.key(), runID, "backlog-reconcile", stageTimeout())
		return err
	})
	return reservation, acquired, err
}

// key addresses the reserved item: legacy (unscoped) when the stage runs
// ungaggled, scoped otherwise.
func (r backlogReconcileReservation) key() claimsclient.Key {
	if r.gaggle == "" {
		return claimsclient.Key{ExternalID: r.itemID}
	}
	return claimsclient.Key{Gaggle: r.gaggle, Provider: r.provider, ExternalID: r.itemID}
}

func releaseBacklogClaimReconciliation(l instance.Layout, reservation backlogReconcileReservation) error {
	ledger, err := openStageClaimLedger(l)
	if err != nil {
		return fmt.Errorf("open claim ledger: %w", err)
	}
	return ledger.Locked(claimContext(), claimLockOperationBacklogReconcile, func(tx claimsclient.Ledger) error {
		return tx.ReleaseScoped(claimContext(), reservation.key(), reservation.runID)
	})
}

func hasReconciledMetadataLabel(item providers.WorkItem) bool {
	return item.HasLabel(providers.LabelClaimed) ||
		item.HasLabel(providers.LabelStale) ||
		item.HasLabel(providers.LabelTracking) ||
		// #3355: an issue carrying ONLY the block marker still needs
		// inspecting, because that marker is exactly the one nothing else can
		// clear. Without this clause the item is never selected and the
		// blocked-on-sibling check below is unreachable — a check that runs on
		// no input is indistinguishable from one that was never written.
		item.HasLabel(blockedOnSiblingLabel) ||
		// #4154: and the same argument for the remediation park, which is the
		// OTHER label nothing else can clear from an issue. An item parked for
		// an infrastructure failure carries only this one — never ready, which
		// park-escalated removed on the way in — so without this clause the
		// infrastructure-park check below runs on no input at all.
		item.HasLabel(needsRemediationLabel) ||
		(item.HasLabel(providers.LabelReady) && itemHasParkLabel(item))
}

// itemHasParkLabel reports whether item carries one of the park dispositions
// (#2028) that cannot coexist with goobers:ready — goobers:needs-human (a
// human decision is pending), goobers:blocked-on-sibling, or
// goobers:needs-remediation.
func itemHasParkLabel(item providers.WorkItem) bool {
	return item.HasLabel(providers.LabelNeedsHuman) ||
		item.HasLabel(blockedOnSiblingLabel) ||
		item.HasLabel(needsRemediationLabel)
}

func inspectBacklogMetadata(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	botLogin string,
	now time.Time,
	stalenessPolicy backlogStalenessPolicy,
	recs map[string]blockedRecord,
) (backlogMetadataCorrection, string, error) {
	correction := backlogMetadataCorrection{}
	validTracking := false
	if item.HasLabel(providers.LabelClaimed) {
		correction.checkClaim = true
	}
	if item.HasLabel(providers.LabelTracking) {
		hasOpenChildren, _, err := trackingItemHasOpenChildren(ctx, provider, repo, item)
		if err != nil {
			return correction, botLogin, fmt.Errorf("inspect tracking children: %w", err)
		}
		if hasOpenChildren {
			validTracking = true
			if item.HasLabel(providers.LabelReady) {
				correction.removeLabels = append(correction.removeLabels, providers.LabelReady)
				correction.reasons = append(correction.reasons, trackingOpenReadyReason)
			}
		} else {
			correction.trackingComplete = true
			correction.removeLabels = append(correction.removeLabels, providers.LabelTracking)
			correction.reasons = append(correction.reasons, trackingCompleteReason)
		}
	}
	// #1911: the ledger is the machine-readable block record and the label is
	// the operator-facing mirror of it; they must not disagree. A marker
	// cleared while the recorded blockers are still open is restored here.
	driftedBlockers, err := driftedBlockedOnSiblingBlockers(ctx, provider, repo, item, recs)
	if err != nil {
		return correction, botLogin, fmt.Errorf("inspect recorded blockers: %w", err)
	}
	if len(driftedBlockers) > 0 {
		correction.addLabels = append(correction.addLabels, blockedOnSiblingLabel)
		correction.reasons = append(correction.reasons, blockedOnSiblingRestoredReason(driftedBlockers))
	}
	// #4154: decided BEFORE the ready-coexistence rule below, because an
	// infrastructure park that is being cleared in this same pass is not a park
	// that goobers:ready has to yield to.
	infraParkCleared, err := staleInfrastructureRemediationPark(ctx, provider, repo, item)
	if err != nil {
		return correction, botLogin, fmt.Errorf("inspect remediation park: %w", err)
	}
	if infraParkCleared {
		correction.removeLabels = append(correction.removeLabels, needsRemediationLabel)
		correction.reasons = append(correction.reasons, infrastructureParkResolvedReason)
	}
	stillParked := item.HasLabel(providers.LabelNeedsHuman) ||
		item.HasLabel(blockedOnSiblingLabel) ||
		(item.HasLabel(needsRemediationLabel) && !infraParkCleared)
	if !validTracking && item.HasLabel(providers.LabelReady) && (stillParked || len(driftedBlockers) > 0) {
		correction.removeLabels = append(correction.removeLabels, providers.LabelReady)
		correction.reasons = append(correction.reasons,
			"removed `goobers:ready` because it cannot coexist with a park disposition "+
				"(`goobers:needs-human`, `goobers:blocked-on-sibling`, or `goobers:needs-remediation`)")
	}
	// #3355: an issue parked on siblings that have all since closed can never
	// shed the label on its own -- the only unpark path iterates pull requests
	// and fires only on a bot PR merging. Clearing it here is fail-closed by
	// design: see staleBlockedOnSiblingMarker.
	if item.HasLabel(blockedOnSiblingLabel) {
		resolved, err := staleBlockedOnSiblingMarker(ctx, provider, repo, item, recs)
		if err != nil {
			return correction, botLogin, fmt.Errorf("inspect blocked-on-sibling blockers: %w", err)
		}
		if resolved {
			correction.removeLabels = append(correction.removeLabels, blockedOnSiblingLabel)
			correction.reasons = append(correction.reasons, blockedOnSiblingResolvedReason)
		}
	}
	if item.HasLabel(providers.LabelStale) {
		reason := ""
		switch {
		case !strings.EqualFold(item.State, "open"):
			reason = "removed `stale` because the issue is no longer open"
		case item.Assignee != "":
			reason = fmt.Sprintf("removed `stale` because the issue now has owner `%s`", item.Assignee)
		default:
			if botLogin == "" {
				var err error
				botLogin, err = provider.AuthenticatedLogin(ctx)
				if err != nil {
					return correction, botLogin, fmt.Errorf("resolve reconciliation actor: %w", err)
				}
			}
			comments, err := provider.ListComments(ctx, repo, item.ID)
			if err != nil {
				return correction, botLogin, fmt.Errorf("inspect stale activity: %w", err)
			}
			signal, err := calculateBacklogStaleness(item, comments, botLogin, now, stalenessPolicy)
			if err != nil {
				return correction, botLogin, fmt.Errorf("calculate stale activity: %w", err)
			}
			if !signal.Stale {
				reason = "removed `stale` because the issue is below the configured staleness threshold"
			}
		}
		if reason != "" {
			correction.removeLabels = append(correction.removeLabels, providers.LabelStale)
			correction.reasons = append(correction.reasons, reason)
		}
	}
	return correction, botLogin, nil
}

func revalidateCompletedTrackingItem(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	itemID string,
	correction backlogMetadataCorrection,
) (backlogMetadataCorrection, error) {
	item, err := provider.GetWorkItem(ctx, repo, itemID)
	if err != nil {
		return correction, err
	}
	if !item.HasLabel(providers.LabelTracking) {
		correction.removeLabels = withoutString(correction.removeLabels, providers.LabelTracking)
		correction.reasons = withoutString(correction.reasons, trackingCompleteReason)
		correction.trackingComplete = false
		return correction, nil
	}
	hasOpenChildren, hasUnverifiedChildren, err := trackingItemHasOpenChildren(ctx, provider, repo, item)
	if err != nil {
		return correction, err
	}
	if hasOpenChildren {
		correction.removeLabels = withoutString(correction.removeLabels, providers.LabelTracking)
		correction.reasons = withoutString(correction.reasons, trackingCompleteReason)
		if item.HasLabel(providers.LabelReady) {
			correction.removeLabels = append(correction.removeLabels, providers.LabelReady)
			correction.reasons = append(correction.reasons, trackingOpenReadyReason)
		}
		correction.trackingComplete = false
		return correction, nil
	}
	if !hasUnverifiedChildren && item.HasLabel(providers.LabelAutoClose) && strings.EqualFold(item.State, "open") {
		correction.closeTrackingParent = true
		correction.reasons = append(correction.reasons, trackingAutoCloseReason)
	}
	return correction, nil
}

func trackingItemHasOpenChildren(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
) (bool, bool, error) {
	native, err := provider.ListWorkItemChildren(ctx, repo, item.ID)
	if err != nil {
		return false, false, err
	}
	seen := make(map[string]bool, len(native))
	for _, child := range native {
		seen[child.ID] = true
		if strings.EqualFold(child.State, "open") {
			return true, false, nil
		}
	}
	hasUnverifiedChildren := false
	for _, id := range trackingChecklistIssueIDs(item.Body) {
		if seen[id] {
			continue
		}
		child, err := provider.GetWorkItem(ctx, repo, id)
		if err != nil {
			if providers.IsNotFoundError(err) {
				hasUnverifiedChildren = true
				continue
			}
			return false, hasUnverifiedChildren, err
		}
		if strings.EqualFold(child.State, "open") {
			return true, hasUnverifiedChildren, nil
		}
	}
	return false, hasUnverifiedChildren, nil
}

func trackingChecklistIssueIDs(body string) []string {
	matches := trackingChecklistIssuePattern.FindAllStringSubmatch(body, -1)
	seen := make(map[string]bool, len(matches))
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		id := match[1]
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func withoutString(values []string, reject string) []string {
	out := values[:0]
	for _, value := range values {
		if value != reject {
			out = append(out, value)
		}
	}
	return out
}

func uniqueSortedLabels(labels []string) []string {
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != "" && !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

func reconciliationComment(reasons []string) string {
	var body strings.Builder
	body.WriteString("Goobers backlog reconciliation corrected metadata drift:\n")
	for _, reason := range reasons {
		body.WriteString("\n- ")
		body.WriteString(strings.TrimSuffix(reason, "."))
		body.WriteString(".")
	}
	body.WriteString("\n\nGround truth came from the claim ledger and current forge issue/child state, not from labels.")
	return body.String()
}
