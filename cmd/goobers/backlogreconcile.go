package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

const defaultStaleAfter = 90 * 24 * time.Hour

var trackingChecklistIssuePattern = regexp.MustCompile(`(?m)^\s*[-*+]\s+\[[ xX]\].*?#([1-9][0-9]*)\b`)

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
	needsClaimSnapshot := false
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
		if correction.checkClaim {
			needsClaimSnapshot = true
		}
		inspected = append(inspected, inspectedBacklogItem{item: current, correction: correction})
	}

	var claimSnapshot []localscheduler.ClaimEntry
	if needsClaimSnapshot {
		lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
		if err := withClaimLock(lockPath, claimLockOperationBacklogReconcile, func() error {
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
			if err != nil {
				return fmt.Errorf("open claim ledger: %w", err)
			}
			claimSnapshot = ledger.Snapshot()
			return nil
		}); err != nil {
			return err
		}
	}

	for _, inspectedItem := range inspected {
		current := inspectedItem.item
		correction := inspectedItem.correction
		if correction.checkClaim &&
			!hasLiveClaim(claimSnapshot, current.ID, providerGaggle(), string(repo.Provider), observedAt) {
			correction.orphanedClaim = true
			correction.removeLabels = append(correction.removeLabels, providers.LabelClaimed)
			correction.reasons = append(correction.reasons,
				"removed `goobers:claimed` because no live claim-ledger lease backs it")
		}
		correction.removeLabels = uniqueSortedLabels(correction.removeLabels)
		if len(correction.removeLabels) == 0 {
			continue
		}
		comment := reconciliationComment(correction.reasons)
		if correction.orphanedClaim {
			if _, err := provider.ReconcileOrphanedWorkItemClaim(
				ctx,
				repo,
				current.ID,
				correction.removeLabels,
				comment,
			); err != nil {
				return fmt.Errorf("reconcile issue #%s: %w", current.ID, err)
			}
			continue
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository:   repo,
			ID:           current.ID,
			RemoveLabels: correction.removeLabels,
			Comment:      comment,
		}); err != nil {
			return fmt.Errorf("reconcile issue #%s: %w", current.ID, err)
		}
	}
	return nil
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

func hasLiveClaim(claims []localscheduler.ClaimEntry, itemID, gaggle, provider string, now time.Time) bool {
	for _, claim := range claims {
		externalID := claim.ExternalID
		if externalID == "" {
			externalID = claim.ItemID
		}
		if externalID != itemID || !claim.ExpiresAt.After(now) {
			continue
		}
		if claim.Gaggle == "" && claim.Provider == "" {
			return true
		}
		if claim.Gaggle == gaggle && claim.Provider == provider {
			return true
		}
	}
	return false
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
