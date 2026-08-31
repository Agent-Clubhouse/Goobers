package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

// latestBlockedOnSiblingState scans comments (oldest first, ListComments' own
// order) for the LAST one carrying a blocked-on-sibling payload apply-verdict
// posted (#747) — only the most recently recorded block is still actionable —
// and returns that comment's ID too, so a caller can edit/clear it in place.
// found is false when no comment in the thread carries the payload (the PR was
// never parked as blocked-on-sibling), not an error. Mirrors
// latestRemediationState's shape (remediationcheckpoint.go).
func latestBlockedOnSiblingState(comments []providers.Comment) (state blockedOnSiblingState, commentID string, found bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if s, ok := parseBlockedOnSiblingComment(comments[i].Body); ok {
			return s, comments[i].ID, true
		}
	}
	return blockedOnSiblingState{}, "", false
}

// recordedBlockedOnSiblingBlockers fails open for an absent record: without
// named blockers there is no concrete condition that can keep a PR parked.
func recordedBlockedOnSiblingBlockers(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) ([]int, error) {
	if !hasAnyLabel(pr.Labels, []string{blockedOnSiblingLabel}) {
		return nil, nil
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return nil, err
	}
	state, _, found := latestBlockedOnSiblingState(comments)
	if !found || len(state.Blockers) == 0 {
		return nil, nil
	}
	return state.Blockers, nil
}

func blockedOnSiblingScanContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, stageTimeout())
}

func namedBlockerStillBlocks(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, blocker int) (bool, error) {
	item, err := provider.GetWorkItem(ctx, repo, strconv.Itoa(blocker))
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(item.State, "open") {
		return false, nil
	}
	if !item.HasLabel(mergeDemotedLabel) {
		return true, nil
	}
	pr, err := provider.GetPullRequest(ctx, repo, strconv.Itoa(blocker))
	if err != nil {
		return false, err
	}
	demoted, err := demotionStillHolds(ctx, provider, repo, pr)
	if err != nil {
		return false, err
	}
	// A live demotion lets successors drain; a stale label does not.
	return !demoted, nil
}

// liveBlockedOnSiblingBlockers returns the named blocker PRs that are still
// effective for a parked PR. A blocker resolves when it closes or while a
// snapshot-valid merge demotion lets its successors drain around it.
func liveBlockedOnSiblingBlockers(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) ([]int, error) {
	blockers, err := recordedBlockedOnSiblingBlockers(ctx, provider, repo, pr)
	if err != nil {
		return nil, err
	}
	return filterLiveBlockedOnSiblingBlockers(ctx, provider, repo, blockers)
}

func liveBlockedOnSiblingState(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (blockedOnSiblingState, error) {
	if !hasAnyLabel(pr.Labels, []string{blockedOnSiblingLabel}) {
		return blockedOnSiblingState{}, nil
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return blockedOnSiblingState{}, err
	}
	state, _, found := latestBlockedOnSiblingState(comments)
	if !found {
		return blockedOnSiblingState{}, nil
	}
	state.Blockers, err = filterLiveBlockedOnSiblingBlockers(ctx, provider, repo, state.Blockers)
	if err != nil {
		return blockedOnSiblingState{}, err
	}
	return state, nil
}

func filterLiveBlockedOnSiblingBlockers(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, blockers []int) ([]int, error) {
	var open []int
	seen := make(map[int]bool)
	for _, blocker := range blockers {
		if seen[blocker] {
			continue
		}
		seen[blocker] = true
		blocks, err := namedBlockerStillBlocks(ctx, provider, repo, blocker)
		if err != nil {
			return nil, err
		}
		if blocks {
			open = append(open, blocker)
		}
	}
	return open, nil
}

// blockedOnSiblingStillBlocks reports whether pr's blocker-aware parking still
// holds (#748). It is also used by post-merge unpark and pr-remediation.
func blockedOnSiblingStillBlocks(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	blockers, err := recordedBlockedOnSiblingBlockers(ctx, provider, repo, pr)
	if err != nil {
		return false, err
	}
	for _, blocker := range blockers {
		blocks, err := namedBlockerStillBlocks(ctx, provider, repo, blocker)
		if err != nil {
			return false, err
		}
		if blocks {
			return true, nil
		}
	}
	return false, nil
}

// blockedOnSiblingResolvedReason explains a cleared marker in the reconcile
// report.
const blockedOnSiblingResolvedReason = "removed `goobers:blocked-on-sibling` because every blocker it named is closed"

// staleBlockedOnSiblingMarker reports whether an ISSUE's blocked-on-sibling
// marker has outlived the blockers it named (#3355).
//
// The PR path has no equivalent problem: post-merge unpark clears the label
// when a merge resolves the last blocker. Issues have no such path at all --
// unparkResolvedSiblings iterates pull requests only, and fires only on a bot
// PR merging, so a blocker closed by hand never triggers anything. 60 open
// issues currently carry this label and none can shed it.
//
// NOTE THE POLARITY, which is deliberately the opposite of
// recordedBlockedOnSiblingBlockers'. That function fails OPEN for an absent
// record -- reasonable when the question is "may this PR be selected", since
// nothing concrete is holding it. Here the question is "should I remove a
// label", which is an action, and roughly half the parked issues record their
// blockers as native GitHub dependencies rather than as this comment payload.
// Failing open would strip the marker from every one of those, unparking
// issues that are genuinely still blocked. So: clear only on positive proof
// that named blockers exist and are all resolved; absent record means no
// action.
//
// It also deliberately does not re-apply goobers:ready. Clearing a stale block
// marker states that a condition no longer holds; deciding an item deserves
// another attempt is a separate judgement that stays with a human (operator
// ruling, 2026-08-22).
//
// The ledger (scheduler/blocked.json) is consulted first and is decisive
// while any blocker it records is still open (#1911). A curation pass that
// resolved an item's dependencies from the prose of an old escalation
// comment, rather than from the recorded blockers, could otherwise conclude
// "unblocked" against blockers that were never the recorded ones — leaving
// the operator-facing label saying ready while filterBlockedEligibility keeps
// excluding the item on a learned block that still holds.
func staleBlockedOnSiblingMarker(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	recs map[string]blockedRecord,
) (bool, error) {
	if !item.HasLabel(blockedOnSiblingLabel) {
		return false, nil
	}
	recorded := recordedLedgerBlockers(recs, repo, item.ID)
	if len(recorded) > 0 {
		live, err := liveLedgerBlockers(ctx, provider, repo, recorded)
		if err != nil {
			return false, err
		}
		if len(live) > 0 {
			return false, nil
		}
	}
	comments, err := provider.ListComments(ctx, repo, item.ID)
	if err != nil {
		return false, err
	}
	state, _, found := latestBlockedOnSiblingState(comments)
	if !found || len(state.Blockers) == 0 {
		// A ledger record naming blockers is itself the positive proof this
		// function requires, so a resolved record clears the marker even when
		// no comment payload was ever posted.
		return len(recorded) > 0, nil
	}
	live, err := filterLiveBlockedOnSiblingBlockers(ctx, provider, repo, state.Blockers)
	if err != nil {
		return false, err
	}
	return len(live) == 0, nil
}

// recordedLedgerBlockers returns the blockers scheduler/blocked.json records
// for itemID in repo, deduplicated and ordered. It is the machine-readable
// block state the operator-facing label must agree with (#1911). An unscoped
// legacy record stays quarantined, matching blockedRecordAppliesToRepository.
func recordedLedgerBlockers(recs map[string]blockedRecord, repo providers.RepositoryRef, itemID string) []string {
	if len(recs) == 0 || itemID == "" {
		return nil
	}
	var blockers []string
	collect := func(rec blockedRecord) {
		for _, blocker := range rec.Blockers {
			if blocker != "" && !slices.Contains(blockers, blocker) {
				blockers = append(blockers, blocker)
			}
		}
	}
	if rec, ok := recs[blockedRecordKey(repo, itemID)]; ok && blockedRecordAppliesToRepository(rec, repo) {
		collect(rec)
	} else {
		for key, rec := range recs {
			if !blockedRecordAppliesToRepository(rec, repo) || blockedRecordItemID(key, rec) != itemID {
				continue
			}
			collect(rec)
		}
	}
	sort.Strings(blockers)
	return blockers
}

// liveLedgerBlockers returns the recorded blockers that are still open. A
// lookup failure is an error rather than an assumed-closed blocker, so an
// unresolvable blocker can never be read as a resolved one.
func liveLedgerBlockers(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, blockers []string) ([]string, error) {
	var live []string
	for _, blocker := range blockers {
		blockerItem, err := provider.GetWorkItem(ctx, repo, blockedLookupID(blocker))
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(blockerItem.State, "open") {
			live = append(live, blocker)
		}
	}
	return live, nil
}

// driftedBlockedOnSiblingBlockers reports the still-open blockers recorded for
// an OPEN item that no longer carries the marker (#1911) — the drifted state
// where the label says unblocked while the learned block still holds. The
// label is the operator-facing signal, so it is restored rather than left
// disagreeing with the ledger.
//
// An item already parked `goobers:needs-human` is left alone: that is the
// stronger disposition, and it is what reconcileBlockedCycleLabels applies to
// the members of a circular dependency, which are recorded as blocked but are
// emphatically not self-clearing.
func driftedBlockedOnSiblingBlockers(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	recs map[string]blockedRecord,
) ([]string, error) {
	if item.HasLabel(blockedOnSiblingLabel) ||
		item.HasLabel(providers.LabelNeedsHuman) ||
		!strings.EqualFold(item.State, "open") {
		return nil, nil
	}
	recorded := recordedLedgerBlockers(recs, repo, item.ID)
	if len(recorded) == 0 {
		return nil, nil
	}
	return liveLedgerBlockers(ctx, provider, repo, recorded)
}

// blockedOnSiblingRestoredReason explains a restored marker in the reconcile
// report, naming the blockers that are still open.
func blockedOnSiblingRestoredReason(blockers []string) string {
	refs := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		refs = append(refs, humanBlockedReference(normalizeBlockedReference(blocker, "").display(false)))
	}
	return fmt.Sprintf(
		"restored `goobers:blocked-on-sibling` because the learned block ledger still records this item blocked on open %s",
		strings.Join(refs, ", "),
	)
}
