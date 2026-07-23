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

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

const defaultStaleAfter = 90 * 24 * time.Hour

var trackingChecklistIssuePattern = regexp.MustCompile(`(?m)^\s*[-*+]\s+\[[ xX]\].*?#([1-9][0-9]*)\b`)

var backlogReconcileReservationSequence atomic.Uint64

type backlogMetadataCorrection struct {
	removeLabels  []string
	reasons       []string
	checkClaim    bool
	orphanedClaim bool
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
	now func() time.Time,
) error {
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository:  repo,
		Labels:      []string{trustLabel},
		State:       "all",
		OldestFirst: true,
	})
	if err != nil {
		return fmt.Errorf("list trusted backlog items: %w", err)
	}

	observedAt := now()
	botLogin := ""
	inspected := make([]inspectedBacklogItem, 0, len(items))
	for _, item := range items {
		if !hasReconciledMetadataLabel(item) {
			continue
		}
		current, err := provider.GetWorkItem(ctx, repo, item.ID)
		if err != nil {
			return fmt.Errorf("refresh issue #%s: %w", item.ID, err)
		}
		correction, login, err := inspectBacklogMetadata(ctx, provider, repo, current, botLogin, observedAt)
		if err != nil {
			return fmt.Errorf("inspect issue #%s: %w", item.ID, err)
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
		var reservation *backlogReconcileReservation
		if correction.checkClaim {
			var acquired bool
			reservation, acquired, err = reserveBacklogClaimReconciliation(l, repo, current.ID, now)
			if err != nil {
				return fmt.Errorf("reserve claim reconciliation for issue #%s: %w", current.ID, err)
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
		if len(correction.removeLabels) == 0 {
			continue
		}
		comment := reconciliationComment(correction.reasons)
		var correctionErr error
		if correction.orphanedClaim {
			_, correctionErr = provider.ReconcileOrphanedWorkItemClaim(
				ctx,
				repo,
				current.ID,
				correction.removeLabels,
				comment,
			)
		} else {
			_, correctionErr = provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository:   repo,
				ID:           current.ID,
				RemoveLabels: correction.removeLabels,
				Comment:      comment,
			})
		}
		if reservation != nil {
			if releaseErr := releaseBacklogClaimReconciliation(l, *reservation); releaseErr != nil {
				correctionErr = errors.Join(correctionErr, fmt.Errorf("release claim-reconciliation reservation: %w", releaseErr))
			}
		}
		if correctionErr != nil {
			return fmt.Errorf("reconcile issue #%s: %w", current.ID, correctionErr)
		}
	}
	return nil
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
	runID := fmt.Sprintf(
		"%s/backlog-reconcile/%d/%d",
		ownerRunID,
		os.Getpid(),
		backlogReconcileReservationSequence.Add(1),
	)
	reservation := &backlogReconcileReservation{
		itemID:   itemID,
		gaggle:   gaggle,
		provider: string(repo.Provider),
		runID:    runID,
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
		if gaggle == "" {
			acquired, _, err = ledger.Claim(itemID, runID, "backlog-reconcile", stageTimeout())
		} else {
			acquired, _, err = ledger.ClaimScoped(localscheduler.ClaimKey{
				Gaggle:     gaggle,
				Provider:   string(repo.Provider),
				ExternalID: itemID,
			}, runID, "backlog-reconcile", stageTimeout())
		}
		return err
	})
	return reservation, acquired, err
}

func releaseBacklogClaimReconciliation(l instance.Layout, reservation backlogReconcileReservation) error {
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBacklogReconcile, func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		if reservation.gaggle == "" {
			return ledger.Release(reservation.itemID, reservation.runID)
		}
		return ledger.ReleaseScoped(localscheduler.ClaimKey{
			Gaggle:     reservation.gaggle,
			Provider:   reservation.provider,
			ExternalID: reservation.itemID,
		}, reservation.runID)
	})
}

func hasReconciledMetadataLabel(item providers.WorkItem) bool {
	return item.HasLabel(providers.LabelClaimed) ||
		item.HasLabel(providers.LabelStale) ||
		item.HasLabel(providers.LabelTracking) ||
		(item.HasLabel(providers.LabelReady) && item.HasLabel(providers.LabelNeedsHuman))
}

func inspectBacklogMetadata(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	botLogin string,
	now time.Time,
) (backlogMetadataCorrection, string, error) {
	correction := backlogMetadataCorrection{}
	validTracking := false
	if item.HasLabel(providers.LabelClaimed) {
		correction.checkClaim = true
	}
	if item.HasLabel(providers.LabelTracking) {
		hasOpenChildren, err := trackingItemHasOpenChildren(ctx, provider, repo, item)
		if err != nil {
			return correction, botLogin, fmt.Errorf("inspect tracking children: %w", err)
		}
		if hasOpenChildren {
			validTracking = true
			if item.HasLabel(providers.LabelReady) {
				correction.removeLabels = append(correction.removeLabels, providers.LabelReady)
				correction.reasons = append(correction.reasons,
					"removed `goobers:ready` because a tracking issue with open children is not directly implementable")
			}
		} else {
			correction.removeLabels = append(correction.removeLabels, providers.LabelTracking)
			correction.reasons = append(correction.reasons,
				"removed `tracking` because the issue has no open provider or checklist children")
		}
	}
	if !validTracking && item.HasLabel(providers.LabelReady) && item.HasLabel(providers.LabelNeedsHuman) {
		correction.removeLabels = append(correction.removeLabels, providers.LabelReady)
		correction.reasons = append(correction.reasons,
			"removed `goobers:ready` because it cannot coexist with the fail-closed `goobers:needs-human` state")
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
			recent, err := hasRecentHumanComment(ctx, provider, repo, item.ID, botLogin, now.Add(-defaultStaleAfter))
			if err != nil {
				return correction, botLogin, fmt.Errorf("inspect stale activity: %w", err)
			}
			if recent {
				reason = "removed `stale` because the issue has recent human activity"
			}
		}
		if reason != "" {
			correction.removeLabels = append(correction.removeLabels, providers.LabelStale)
			correction.reasons = append(correction.reasons, reason)
		}
	}
	return correction, botLogin, nil
}

func trackingItemHasOpenChildren(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
) (bool, error) {
	native, err := provider.ListWorkItemChildren(ctx, repo, item.ID)
	if err != nil {
		return false, err
	}
	seen := make(map[string]bool, len(native))
	for _, child := range native {
		seen[child.ID] = true
		if strings.EqualFold(child.State, "open") {
			return true, nil
		}
	}
	for _, id := range trackingChecklistIssueIDs(item.Body) {
		if seen[id] {
			continue
		}
		child, err := provider.GetWorkItem(ctx, repo, id)
		if err != nil {
			if providers.IsNotFoundError(err) {
				continue
			}
			return false, err
		}
		if strings.EqualFold(child.State, "open") {
			return true, nil
		}
	}
	return false, nil
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

func hasRecentHumanComment(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	id string,
	botLogin string,
	cutoff time.Time,
) (bool, error) {
	comments, err := provider.ListComments(ctx, repo, id)
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		if comment.CreatedAt != nil &&
			!comment.CreatedAt.Before(cutoff) &&
			!strings.EqualFold(comment.AuthorType, "bot") &&
			!strings.EqualFold(comment.Author, botLogin) {
			return true, nil
		}
	}
	return false, nil
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
