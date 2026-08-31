package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
	// backlog is the authoritative claim scope when it could be resolved.
	backlog *apiv1.BacklogIdentity
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
	botLogin := ""
	reconciled := 0
	inspected := make([]inspectedBacklogItem, 0, len(items))
	for _, item := range items {
		if !hasReconciledMetadataLabel(item) {
			continue
		}
		current, err := provider.GetWorkItem(ctx, repo, item.ID)
		if err != nil {
			return reconciled, fmt.Errorf("refresh issue #%s: %w", item.ID, err)
		}
		correction, login, err := inspectBacklogMetadata(ctx, provider, repo, current, botLogin, observedAt, stalenessPolicy)
		if err != nil {
			return reconciled, fmt.Errorf("inspect issue #%s: %w", item.ID, err)
		}
		botLogin = login
		if !correction.checkClaim && len(correction.removeLabels) == 0 {
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
		if len(correction.removeLabels) == 0 && !correction.closeTrackingParent {
			continue
		}
		comment := reconciliationComment(correction.reasons)
		state := ""
		if correction.closeTrackingParent {
			state = "closed"
		}
		var correctionErr error
		if correction.orphanedClaim {
			if correction.closeTrackingParent {
				_, correctionErr = provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
					Repository: repo,
					ID:         current.ID,
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
	ownerRunID := os.Getenv("GOOBERS_RUN_ID")
	if ownerRunID == "" {
		ownerRunID = "standalone"
	}
	runID := formatBacklogReconcileRunID(ownerRunID, os.Getpid(), backlogReconcileReservationSequence.Add(1))
	reservation := &backlogReconcileReservation{
		itemID:   itemID,
		gaggle:   gaggle,
		provider: string(repo.Provider),
		runID:    runID,
	}
	// Reconciliation reserves the item under the SAME authoritative scope a
	// real claim uses, so it cannot repair metadata for an item another gaggle
	// sharing this backlog is actively working. Like the claim path, backlog
	// scope applies only to a gaggle-owned run; a standalone invocation keeps
	// the historical key. An unresolvable identity falls back to gaggle scope
	// rather than failing the repair pass: reconciliation is best-effort
	// maintenance, and refusing to run would be a worse outcome than reserving
	// slightly too narrowly.
	if gaggle != "" {
		if identity, err := backlogIdentityForStage(l.Root, repo); err == nil {
			reservation.backlog = &identity
		}
	}
	acquired := false
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBacklogReconcile, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(l.SchedulerDir(), claimLedgerFileName),
			localscheduler.WithLedgerClock(now),
		)
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		if key, scoped := reservation.claimKey(); scoped {
			acquired, _, err = ledger.ClaimScoped(key, runID, "backlog-reconcile", stageTimeout())
		} else {
			acquired, _, err = ledger.Claim(itemID, runID, "backlog-reconcile", stageTimeout())
		}
		return err
	})
	return reservation, acquired, err
}

// claimKey returns the reservation's ledger key and whether it is scoped at
// all (a gaggle-less standalone invocation keeps the item-only key).
func (r backlogReconcileReservation) claimKey() (localscheduler.ClaimKey, bool) {
	if r.backlog != nil {
		return backlogClaimKey(*r.backlog, r.gaggle, r.itemID), true
	}
	if r.gaggle == "" {
		return localscheduler.ClaimKey{}, false
	}
	return localscheduler.ClaimKey{
		Gaggle:     r.gaggle,
		Provider:   r.provider,
		ExternalID: r.itemID,
	}, true
}

func releaseBacklogClaimReconciliation(l instance.Layout, reservation backlogReconcileReservation) error {
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBacklogReconcile, func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		if key, scoped := reservation.claimKey(); scoped {
			return ledger.ReleaseScoped(key, reservation.runID)
		}
		return ledger.Release(reservation.itemID, reservation.runID)
	})
}

func hasReconciledMetadataLabel(item providers.WorkItem) bool {
	return item.HasLabel(providers.LabelClaimed) ||
		item.HasLabel(providers.LabelStale) ||
		item.HasLabel(providers.LabelTracking) ||
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
	if !validTracking && item.HasLabel(providers.LabelReady) && itemHasParkLabel(item) {
		correction.removeLabels = append(correction.removeLabels, providers.LabelReady)
		correction.reasons = append(correction.reasons,
			"removed `goobers:ready` because it cannot coexist with a park disposition "+
				"(`goobers:needs-human`, `goobers:blocked-on-sibling`, or `goobers:needs-remediation`)")
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
