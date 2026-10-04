package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/internal/stateclient"
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

const (
	defaultBacklogReconcileScanBudget = 100
	defaultBacklogReconcileTimeBudget = 4 * time.Minute
	backlogReconcileCursorSchema      = "goobers.dev/backlog-reconcile-cursor/v1"
	backlogReconcilePhaseOpen         = "open"
	backlogReconcilePhaseClosed       = "closed"
	backlogReconcilePhaseClaims       = "claims"
	backlogChildInspectionPhaseVerify = "verify"
	backlogChildInspectionPhaseDone   = "done"
)

var errBacklogReconcileBudgetExhausted = errors.New("backlog reconciliation budget exhausted")

type backlogMetadataCorrection struct {
	addLabels           []string
	removeLabels        []string
	reasons             []string
	checkClaim          bool
	orphanedClaim       bool
	claimEpochRunID     string
	trackingComplete    bool
	closeTrackingParent bool
	childCursor         *backlogChildInspectionCursor
}

type backlogChildInspectionBudgetError struct {
	cursor backlogChildInspectionCursor
}

func (e backlogChildInspectionBudgetError) Error() string {
	return errBacklogReconcileBudgetExhausted.Error()
}

func (e backlogChildInspectionBudgetError) Unwrap() error {
	return errBacklogReconcileBudgetExhausted
}

type backlogReconciliationResult struct {
	Reconciled int                  `json:"reconciled"`
	Scan       backlogReconcileScan `json:"scan"`

	cursorKey      string
	observedCursor backlogReconcileCursorState
	nextCursor     backlogReconcileCursorState
}

type backlogReconcileScan struct {
	Budget         int    `json:"budget"`
	Examined       int    `json:"examined"`
	Spent          int    `json:"spent"`
	Complete       bool   `json:"complete"`
	WorkRemaining  bool   `json:"workRemaining"`
	NextPhase      string `json:"nextPhase,omitempty"`
	NextCursor     string `json:"nextCursor,omitempty"`
	OpenExamined   int    `json:"openExamined,omitempty"`
	ClosedExamined int    `json:"closedExamined,omitempty"`
	ClaimExamined  int    `json:"claimExamined,omitempty"`
}

type backlogReconcileCursorState struct {
	Schema         string            `json:"schema"`
	Gaggle         string            `json:"gaggle,omitempty"`
	Provider       string            `json:"provider"`
	Repository     string            `json:"repository"`
	TrustLabel     string            `json:"trustLabel"`
	AssignedTo     string            `json:"assignedTo,omitempty"`
	ScopedAssignee bool              `json:"scopedAssignee,omitempty"`
	Ownership      ownershipScopeKey `json:"ownership,omitempty"`
	ThresholdDays  int               `json:"thresholdDays"`
	Phase          string            `json:"phase"`
	Open           backlogScanCursor `json:"open,omitempty"`
	Closed         backlogScanCursor `json:"closed,omitempty"`
	Claim          string            `json:"claim,omitempty"`
}

type backlogReconcileAssigneeScope struct {
	respectAssignee bool
	assignedTo      string
	ownership       issueOwnershipScope
}

type ownershipScopeKey struct {
	Assignees  []string `json:"assignees,omitempty"`
	Unassigned string   `json:"unassigned,omitempty"`
}

func (s backlogReconcileAssigneeScope) permits(item providers.WorkItem) bool {
	if s.respectAssignee && !item.AssigneeMatches(s.assignedTo) {
		return false
	}
	return s.ownership.permits(item)
}

func (s backlogReconcileAssigneeScope) queryAssignee() string {
	if s.respectAssignee && s.assignedTo != "" {
		return s.assignedTo
	}
	if !s.respectAssignee && len(s.ownership.assignees) == 1 && s.ownership.unassigned == ownershipUnassignedRefuse {
		return s.ownership.assignees[0]
	}
	return ""
}

func (s backlogReconcileAssigneeScope) key() ownershipScopeKey {
	return ownershipScopeKey{Assignees: append([]string(nil), s.ownership.assignees...), Unassigned: s.ownership.unassigned}
}

type backlogReconcileReservation struct {
	itemID   string
	gaggle   string
	provider string
	runID    string
}

type backlogReconcileBudget struct {
	limit            int
	spent            int
	deadline         time.Time
	now              func() time.Time
	reservedRequests int
}

func newBacklogReconcileBudget(limit int, startedAt time.Time, now func() time.Time) *backlogReconcileBudget {
	return &backlogReconcileBudget{
		limit:    limit,
		deadline: startedAt.Add(defaultBacklogReconcileTimeBudget),
		now:      now,
	}
}

func (b *backlogReconcileBudget) Remaining() int {
	if b == nil {
		return 1 << 30
	}
	return b.limit - b.spent
}

func (b *backlogReconcileBudget) Spent() int {
	if b == nil {
		return 0
	}
	return b.spent
}

func (b *backlogReconcileBudget) Available() bool {
	if b == nil {
		return true
	}
	return b.Remaining() > 0 && b.now().Before(b.deadline)
}

func (b *backlogReconcileBudget) Consume() error {
	if b == nil {
		return nil
	}
	if b.Remaining() <= 0 {
		return errBacklogReconcileBudgetExhausted
	}
	if b.reservedRequests == 0 && !b.now().Before(b.deadline) {
		return errBacklogReconcileBudgetExhausted
	}
	b.spent++
	if b.reservedRequests > 0 {
		b.reservedRequests--
	}
	return nil
}

func (b *backlogReconcileBudget) EnsureRemaining(requests int) error {
	if b == nil {
		return nil
	}
	if requests < 0 {
		requests = 0
	}
	if !b.now().Before(b.deadline) || b.Remaining() < requests {
		return errBacklogReconcileBudgetExhausted
	}
	return nil
}

func (b *backlogReconcileBudget) Reserve(requests int) (func(), error) {
	if b == nil {
		return func() {}, nil
	}
	if err := b.EnsureRemaining(requests); err != nil {
		return nil, err
	}
	previous := b.reservedRequests
	b.reservedRequests += requests
	return func() {
		if b.reservedRequests > previous {
			b.reservedRequests = previous
		}
	}, nil
}

func (b *backlogReconcileBudget) PartialReason() string {
	if b == nil {
		return ""
	}
	if b.Remaining() <= 0 {
		return "request budget exhausted"
	}
	if !b.now().Before(b.deadline) {
		return "elapsed time budget exhausted"
	}
	return ""
}

type backlogReconcileBudgetedHTTPClient struct {
	inner  providers.HTTPClient
	budget *backlogReconcileBudget
}

func (c backlogReconcileBudgetedHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if err := c.budget.Consume(); err != nil {
		return nil, err
	}
	if c.inner != nil {
		return c.inner.Do(req)
	}
	return http.DefaultClient.Do(req)
}

func installBacklogReconcileBudget(provider *providers.GitHubProvider, budget *backlogReconcileBudget) func() {
	if provider == nil || budget == nil {
		return func() {}
	}
	previous := provider.Client
	provider.Client = backlogReconcileBudgetedHTTPClient{inner: previous, budget: budget}
	return func() { provider.Client = previous }
}

func reconcileBacklogMetadataDetailed(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	trustLabel string,
	stalenessPolicy backlogStalenessPolicy,
	now func() time.Time,
	scopes ...backlogReconcileAssigneeScope,
) (backlogReconciliationResult, error) {
	result := backlogReconciliationResult{}
	scope := backlogReconcileAssigneeScope{}
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	// Reap terminal and expired ledger leases before inspecting provider labels.
	// This makes the ledger's liveness decision available to the provider-marker
	// reconciliation below, so a dead claimant cannot keep its marker forever.
	// Through the seam (staleclaimrecovery.go): the daemon runs the sweep when
	// this stage is pod-dispatched and has no instance root of its own.
	if err := recoverStageClaims(l, now()); err != nil {
		return result, fmt.Errorf("recover stale claims before metadata reconciliation: %w", err)
	}
	observedAt := now()
	result.Scan = backlogReconcileScan{Budget: backlogReconcileScanBudget()}
	budget := newBacklogReconcileBudget(backlogReconcileMetadataBudget(result.Scan.Budget), observedAt, now)
	restoreClient := installBacklogReconcileBudget(provider, budget)
	defer restoreClient()
	cursorKey := backlogReconcileCursorKey(repo, providerGaggle(), trustLabel, stalenessPolicy, scope)
	result.cursorKey = cursorKey
	store, err := openStageStateStore(l)
	if err != nil {
		return result, fmt.Errorf("open scheduler state: %w", err)
	}
	observedCursor, err := readBacklogReconcileCursor(ctx, store, cursorKey, repo, providerGaggle(), trustLabel, stalenessPolicy, scope)
	if err != nil {
		return result, err
	}
	nextCursor := observedCursor
	nextCursor.Phase = backlogReconcilePhaseOpen
	result.observedCursor = observedCursor
	result.nextCursor = nextCursor
	blockedRecords, err := snapshotBlockedRecords(l)
	if err != nil {
		return result, fmt.Errorf("snapshot learned block ledger: %w", err)
	}
	botLogin := ""
	openLimit := (budget.limit + 1) / 2
	if openLimit < 1 && budget.limit > 0 {
		openLimit = 1
	}
	open, err := reconcileBacklogMetadataPhase(ctx, l, provider, repo, trustLabel, backlogReconcilePhaseOpen, stalenessPolicy, observedAt, scope, budget, openLimit, observedCursor.Open, blockedRecords, &botLogin, now)
	result.Reconciled += open.Reconciled
	result.Scan.Examined += open.Examined
	result.Scan.OpenExamined += open.Examined
	result.Scan.Spent = budget.Spent()
	nextCursor.Open = open.Cursor
	result.nextCursor = nextCursor
	if err != nil && !errors.Is(err, errBacklogReconcileBudgetExhausted) {
		_ = advanceBacklogReconcileCursor(ctx, l, cursorKey, observedCursor, nextCursor)
		return result, err
	}
	if open.Truncated || errors.Is(err, errBacklogReconcileBudgetExhausted) {
		result.Scan.WorkRemaining = true
		result.Scan.NextPhase = backlogReconcilePhaseOpen
		result.Scan.NextCursor = open.Cursor.Cursor
	}
	remainingPhaseBudget := budget.limit - budget.Spent()
	if remainingPhaseBudget > 0 {
		closed, closedErr := reconcileBacklogMetadataPhase(ctx, l, provider, repo, trustLabel, backlogReconcilePhaseClosed, stalenessPolicy, observedAt, scope, budget, remainingPhaseBudget, observedCursor.Closed, blockedRecords, &botLogin, now)
		result.Reconciled += closed.Reconciled
		result.Scan.Examined += closed.Examined
		result.Scan.ClosedExamined += closed.Examined
		result.Scan.Spent = budget.Spent()
		nextCursor.Closed = closed.Cursor
		result.nextCursor = nextCursor
		if closedErr != nil && !errors.Is(closedErr, errBacklogReconcileBudgetExhausted) {
			_ = advanceBacklogReconcileCursor(ctx, l, cursorKey, observedCursor, nextCursor)
			return result, closedErr
		}
		if closed.Truncated || errors.Is(closedErr, errBacklogReconcileBudgetExhausted) {
			result.Scan.WorkRemaining = true
			if result.Scan.NextPhase == "" {
				result.Scan.NextPhase = backlogReconcilePhaseClosed
				result.Scan.NextCursor = closed.Cursor.Cursor
			}
		}
	} else if result.Scan.NextPhase == "" {
		result.Scan.WorkRemaining = true
		result.Scan.NextPhase = backlogReconcilePhaseClosed
		result.Scan.NextCursor = observedCursor.Closed.Cursor
	}
	result.Scan.Complete = !result.Scan.WorkRemaining
	if err := advanceBacklogReconcileCursor(ctx, l, cursorKey, observedCursor, nextCursor); err != nil {
		return result, fmt.Errorf("advance backlog reconciliation cursor: %w", err)
	}
	return result, nil
}

type backlogReconcilePhaseResult struct {
	Cursor     backlogScanCursor
	Examined   int
	Reconciled int
	Truncated  bool
}

//complexitygate:allow bounded phase/cursor state machine with explicit budget exits
func reconcileBacklogMetadataPhase(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	trustLabel, phase string,
	stalenessPolicy backlogStalenessPolicy,
	observedAt time.Time,
	scope backlogReconcileAssigneeScope,
	budget *backlogReconcileBudget,
	phaseBudget int,
	cursor backlogScanCursor,
	blockedRecords map[string]blockedRecord,
	botLogin *string,
	now func() time.Time,
) (backlogReconcilePhaseResult, error) {
	result := backlogReconcilePhaseResult{Cursor: cursor}
	if phaseBudget <= 0 {
		result.Truncated = true
		return result, errBacklogReconcileBudgetExhausted
	}
	state := phase
	var updatedSince *time.Time
	if phase == backlogReconcilePhaseClosed {
		closedSince := observedAt.Add(-stalenessPolicy.threshold())
		updatedSince = &closedSince
	} else {
		state = backlogReconcilePhaseOpen
	}
	stopCursor := cursor.Cursor
	wrapped := false
	skippedDeferred := false
	seen := map[string]bool{}
	phaseStartSpent := budget.Spent()
	queryAssignee := scope.queryAssignee()
	for budget.Available() && budget.Spent()-phaseStartSpent < phaseBudget {
		pageInfo := &providers.ListWorkItemsPageInfo{}
		items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
			Repository:   repo,
			Labels:       []string{trustLabel},
			State:        state,
			Assignee:     queryAssignee,
			UpdatedSince: updatedSince,
			Limit:        1,
			Cursor:       result.Cursor.Cursor,
			PageInfo:     pageInfo,
			OldestFirst:  true,
		})
		if err != nil {
			return result, fmt.Errorf("list %s backlog item after cursor %q: %w", phase, result.Cursor.Cursor, err)
		}
		if pageInfo.CandidateCount < 0 {
			return result, fmt.Errorf("provider returned invalid work-item candidate count %d", pageInfo.CandidateCount)
		}
		next := backlogScanCursor{}
		if pageInfo.HasNext {
			if pageInfo.CandidateCount == 0 || pageInfo.NextCursor == "" {
				return result, fmt.Errorf("provider returned a non-advancing work-item cursor")
			}
			next.Cursor = pageInfo.NextCursor
		}
		if len(items) == 0 {
			if !pageInfo.HasNext {
				if result.Cursor.Cursor != "" && !wrapped && budget.Available() && budget.Spent()-phaseStartSpent < phaseBudget {
					result.Cursor = backlogScanCursor{Deferred: result.Cursor.Deferred, Child: result.Cursor.Child}
					wrapped = true
					continue
				}
				return finishBacklogReconcilePhase(result, skippedDeferred), nil
			}
			if pageInfo.NextCursor == result.Cursor.Cursor {
				if result.Cursor.Cursor != "" && !wrapped && budget.Available() && budget.Spent()-phaseStartSpent < phaseBudget {
					result.Cursor = backlogScanCursor{Deferred: result.Cursor.Deferred, Child: result.Cursor.Child}
					wrapped = true
					continue
				}
				return finishBacklogReconcilePhase(result, skippedDeferred), nil
			}
			result.Cursor = carryDeferredBacklogCursor(result.Cursor, next)
			continue
		}
		item := items[0]
		if !scope.permits(item) {
			result.Examined++
			result.Cursor = carryDeferredBacklogCursor(result.Cursor, next)
			if !pageInfo.HasNext {
				if cursor.Cursor != "" && !wrapped && budget.Available() && budget.Spent()-phaseStartSpent < phaseBudget {
					result.Cursor = backlogScanCursor{Deferred: result.Cursor.Deferred, Child: result.Cursor.Child}
					wrapped = true
					continue
				}
				result.Cursor = backlogScanCursor{}
				return finishBacklogReconcilePhase(result, skippedDeferred), nil
			}
			continue
		}
		if result.Cursor.Deferred == item.ID {
			skippedDeferred = true
			result.Examined++
			child := result.Cursor.Child
			result.Cursor = next
			if child != nil && child.ParentID == item.ID {
				result.Cursor.Child = child
			}
			if !pageInfo.HasNext {
				result.Cursor = backlogScanCursor{Child: child}
				return finishBacklogReconcilePhase(result, skippedDeferred), nil
			}
			if child != nil && child.ParentID == item.ID && next.Cursor == cursor.Cursor {
				result.Cursor = backlogScanCursor{Child: child}
				return finishBacklogReconcilePhase(result, skippedDeferred), nil
			}
			continue
		}
		if seen[item.ID] || (wrapped && backlogReconcileCursorReached(next.Cursor, stopCursor)) {
			result.Cursor = backlogScanCursor{}
			return finishBacklogReconcilePhase(result, skippedDeferred), nil
		}
		seen[item.ID] = true
		if budget.Spent()-phaseStartSpent >= phaseBudget || !budget.Available() {
			result.Examined++
			result.Cursor = deferredBacklogScanCursor(next, item.ID)
			result.Truncated = true
			return result, errBacklogReconcileBudgetExhausted
		}
		childCursor := result.Cursor.Child
		if childCursor != nil && childCursor.ParentID != item.ID {
			childCursor = nil
		}
		reconciled, err := reconcileBacklogMetadataItem(ctx, l, provider, repo, item, observedAt, stalenessPolicy, scope, blockedRecords, botLogin, budget, now, childCursor)
		if err != nil {
			if errors.Is(err, errBacklogReconcileBudgetExhausted) {
				result.Examined++
				result.Cursor = deferredBacklogScanCursor(next, item.ID)
				var childErr backlogChildInspectionBudgetError
				if errors.As(err, &childErr) {
					result.Cursor.Child = &childErr.cursor
				}
				result.Truncated = true
			}
			return result, err
		}
		result.Examined++
		if reconciled {
			result.Reconciled++
		}
		result.Cursor = carryDeferredBacklogCursor(result.Cursor, next)
		if result.Cursor.Child != nil && result.Cursor.Child.ParentID == item.ID {
			result.Cursor.Child = nil
		}
		if !pageInfo.HasNext {
			if cursor.Cursor != "" && !wrapped && budget.Available() && budget.Spent()-phaseStartSpent < phaseBudget {
				result.Cursor = backlogScanCursor{Deferred: result.Cursor.Deferred, Child: result.Cursor.Child}
				wrapped = true
				continue
			}
			result.Cursor = backlogScanCursor{}
			return finishBacklogReconcilePhase(result, skippedDeferred), nil
		}
	}
	result.Truncated = true
	if reason := budget.PartialReason(); reason != "" {
		return result, errBacklogReconcileBudgetExhausted
	}
	return result, nil
}

func finishBacklogReconcilePhase(result backlogReconcilePhaseResult, skippedDeferred bool) backlogReconcilePhaseResult {
	if skippedDeferred {
		result.Truncated = true
	}
	return result
}

func deferredBacklogScanCursor(next backlogScanCursor, itemID string) backlogScanCursor {
	next.Deferred = itemID
	return next
}

func carryDeferredBacklogCursor(current, next backlogScanCursor) backlogScanCursor {
	next.Deferred = current.Deferred
	next.Child = current.Child
	return next
}

func reconcileBacklogMetadataItem(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	observedAt time.Time,
	stalenessPolicy backlogStalenessPolicy,
	scope backlogReconcileAssigneeScope,
	blockedRecords map[string]blockedRecord,
	botLogin *string,
	budget *backlogReconcileBudget,
	now func() time.Time,
	childCursor *backlogChildInspectionCursor,
) (bool, error) {
	if !hasReconciledMetadataLabel(item) && len(recordedLedgerBlockers(blockedRecords, repo, item.ID)) == 0 {
		return false, nil
	}
	current, err := provider.GetWorkItem(ctx, repo, item.ID)
	if err != nil {
		return false, fmt.Errorf("refresh issue #%s: %w", item.ID, err)
	}
	correction, login, err := inspectBacklogMetadataWithChildCursor(ctx, provider, repo, current, *botLogin, observedAt, stalenessPolicy, blockedRecords, childCursor, budget)
	if err != nil {
		return false, fmt.Errorf("inspect issue #%s: %w", item.ID, err)
	}
	*botLogin = login
	if backlogMetadataCorrectionEmpty(correction) {
		return false, nil
	}
	current, err = provider.GetWorkItem(ctx, repo, current.ID)
	if err != nil {
		return false, fmt.Errorf("refresh issue #%s before reconcile: %w", item.ID, err)
	}
	if !scope.permits(current) {
		return false, nil
	}
	correction, login, err = inspectBacklogMetadataWithChildCursor(ctx, provider, repo, current, *botLogin, observedAt, stalenessPolicy, blockedRecords, childCursor, budget)
	if err != nil {
		return false, fmt.Errorf("reinspect issue #%s before reconcile: %w", current.ID, err)
	}
	*botLogin = login
	if backlogMetadataCorrectionEmpty(correction) {
		return false, nil
	}
	if correction.trackingComplete {
		correction, err = revalidateCompletedTrackingItem(ctx, provider, repo, current.ID, correction, budget, childCursor)
		if err != nil {
			return false, fmt.Errorf("revalidate tracking issue #%s: %w", current.ID, err)
		}
	}
	var reservation *backlogReconcileReservation
	if correction.checkClaim {
		reservation, err = reserveOwnedOrphanedClaim(ctx, l, provider, repo, current.ID, now, &correction, budget)
		if err != nil {
			return false, err
		}
	}
	correction.removeLabels = uniqueSortedLabels(correction.removeLabels)
	correction.addLabels = uniqueSortedLabels(correction.addLabels)
	if backlogMetadataCorrectionNoop(correction) {
		return false, nil
	}
	correctionErr := applyBacklogMetadataCorrection(ctx, provider, repo, current, correction, budget)
	if errors.Is(correctionErr, providers.ErrClaimEpochNotOwned) {
		correctionErr = nil
	}
	if errors.Is(correctionErr, errBacklogReconcileBudgetExhausted) && correction.childCursor != nil {
		cursor := *correction.childCursor
		if cursor.Phase == backlogChildInspectionPhaseDone {
			cursor.Phase = backlogChildInspectionPhaseVerify
			cursor.NextIndex = 0
		}
		correctionErr = backlogChildInspectionBudgetError{cursor: cursor}
	}
	if reservation != nil {
		if releaseErr := releaseBacklogClaimReconciliation(l, *reservation); releaseErr != nil {
			correctionErr = errors.Join(correctionErr, fmt.Errorf("release claim-reconciliation reservation: %w", releaseErr))
		}
	}
	if correctionErr != nil {
		return false, fmt.Errorf("reconcile issue #%s: %w", current.ID, correctionErr)
	}
	return true, nil
}

func backlogMetadataCorrectionEmpty(correction backlogMetadataCorrection) bool {
	return !correction.checkClaim && len(correction.removeLabels) == 0 && len(correction.addLabels) == 0
}

func backlogMetadataCorrectionNoop(correction backlogMetadataCorrection) bool {
	return len(correction.removeLabels) == 0 && len(correction.addLabels) == 0 && !correction.closeTrackingParent
}

func applyBacklogMetadataCorrection(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	current providers.WorkItem,
	correction backlogMetadataCorrection,
	budget *backlogReconcileBudget,
) error {
	comment := reconciliationComment(correction.reasons)
	state := ""
	if correction.closeTrackingParent {
		state = "closed"
	}
	if correction.orphanedClaim {
		claimed, err := revalidateBacklogOrphanedClaimEpoch(ctx, provider, repo, current.ID, correction.claimEpochRunID)
		if err != nil {
			return err
		}
		if claimed {
			comment = backlogClaimReleaseBreadcrumb(correction.claimEpochRunID) + "\n\n" + comment
		}
		req := providers.UpdateWorkItemRequest{
			Repository:       repo,
			ID:               current.ID,
			ExpectedRevision: current.Revision,
			AddLabels:        correction.addLabels,
			RemoveLabels:     correction.removeLabels,
			State:            state,
			Comment:          comment,
		}
		_, err = updateBacklogWorkItem(ctx, provider, budget, req)
		return err
	}
	req := providers.UpdateWorkItemRequest{
		Repository:       repo,
		ID:               current.ID,
		ExpectedRevision: current.Revision,
		AddLabels:        correction.addLabels,
		RemoveLabels:     correction.removeLabels,
		State:            state,
		Comment:          comment,
	}
	_, err := updateBacklogWorkItem(ctx, provider, budget, req)
	return err
}

func updateBacklogWorkItem(ctx context.Context, provider *providers.GitHubProvider, budget *backlogReconcileBudget, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	release, err := budget.Reserve(updateWorkItemRequestCost(req))
	if err != nil {
		return providers.WorkItem{}, err
	}
	defer release()
	return provider.UpdateWorkItem(ctx, req)
}

func revalidateBacklogOrphanedClaimEpoch(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, itemID, ownedRunID string) (bool, error) {
	epochs, err := provider.OpenClaimEpochs(ctx, repo, itemID)
	if err != nil {
		return false, err
	}
	for _, epoch := range epochs {
		if !epoch.Trusted || epoch.RunID != ownedRunID {
			return false, providers.ErrClaimEpochNotOwned
		}
	}
	return ownedRunID != "" && len(epochs) > 0, nil
}

func updateWorkItemRequestCost(req providers.UpdateWorkItemRequest) int {
	cost := 2 // before and final GetWorkItem calls.
	if req.Title != nil || req.Body != nil || req.Assignee != nil || req.Milestone != nil || req.State != "" {
		cost++
	}
	if req.Comment != "" {
		cost++
	}
	if len(uniqueSortedLabels(req.AddLabels)) > 0 {
		cost++
	}
	cost += len(uniqueSortedLabels(req.RemoveLabels))
	return cost
}

func backlogClaimReleaseBreadcrumb(runID string) string {
	return fmt.Sprintf("goobers-claim-release: run=%s\n\nReleased by Goobers run `%s`; a later run may claim this item.", runID, runID)
}

func backlogReconcileMetadataBudget(total int) int {
	if total > 1 {
		return total - max(1, total/10)
	}
	return total
}

type invisibleClaimScanResult struct {
	Restored   int
	Examined   int
	Spent      int
	Complete   bool
	NextCursor string
}

func backlogReconcileCursorReached(cursor, stop string) bool {
	if stop == "" {
		return false
	}
	current, errCurrent := strconv.Atoi(cursor)
	target, errTarget := strconv.Atoi(stop)
	if errCurrent != nil || errTarget != nil {
		return cursor == stop
	}
	return current >= target
}

func backlogReconcileScanBudget() int {
	const minBacklogReconcileScanBudget = 10
	raw := strings.TrimSpace(providerInput("reconcileScanLimit", ""))
	if raw == "" {
		return defaultBacklogReconcileScanBudget
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return defaultBacklogReconcileScanBudget
	}
	if limit < minBacklogReconcileScanBudget {
		return minBacklogReconcileScanBudget
	}
	return limit
}

func backlogReconcileCursorKey(repo providers.RepositoryRef, gaggle, trustLabel string, stalenessPolicy backlogStalenessPolicy, scope backlogReconcileAssigneeScope) string {
	key, _ := json.Marshal(struct {
		Repository     providers.RepositoryRef `json:"repository"`
		Gaggle         string                  `json:"gaggle,omitempty"`
		TrustLabel     string                  `json:"trustLabel"`
		AssignedTo     string                  `json:"assignedTo,omitempty"`
		ScopedAssignee bool                    `json:"scopedAssignee,omitempty"`
		Ownership      ownershipScopeKey       `json:"ownership,omitempty"`
		ThresholdDays  int                     `json:"thresholdDays"`
	}{
		Repository:     repo,
		Gaggle:         gaggle,
		TrustLabel:     trustLabel,
		AssignedTo:     scope.assignedTo,
		ScopedAssignee: scope.respectAssignee,
		Ownership:      scope.key(),
		ThresholdDays:  stalenessPolicy.thresholdDays,
	})
	sum := sha256.Sum256(key)
	return stateclient.ReconcileCursorKey(fmt.Sprintf("%x", sum))
}

func readBacklogReconcileCursor(
	ctx context.Context,
	store stateclient.Store,
	key string,
	repo providers.RepositoryRef,
	gaggle, trustLabel string,
	stalenessPolicy backlogStalenessPolicy,
	scope backlogReconcileAssigneeScope,
) (backlogReconcileCursorState, error) {
	value, err := store.Get(ctx, key)
	if err != nil {
		return backlogReconcileCursorState{}, err
	}
	if !value.Exists() {
		return newBacklogReconcileCursor(repo, gaggle, trustLabel, stalenessPolicy, scope), nil
	}
	var cursor backlogReconcileCursorState
	if err := json.Unmarshal(value.Data, &cursor); err != nil {
		return backlogReconcileCursorState{}, fmt.Errorf("decode backlog reconciliation cursor: %w", err)
	}
	want := newBacklogReconcileCursor(repo, gaggle, trustLabel, stalenessPolicy, scope)
	if cursor.Schema != want.Schema ||
		cursor.Gaggle != want.Gaggle ||
		cursor.Provider != want.Provider ||
		cursor.Repository != want.Repository ||
		cursor.TrustLabel != want.TrustLabel ||
		cursor.AssignedTo != want.AssignedTo ||
		cursor.ScopedAssignee != want.ScopedAssignee ||
		!slices.Equal(cursor.Ownership.Assignees, want.Ownership.Assignees) ||
		cursor.Ownership.Unassigned != want.Ownership.Unassigned ||
		cursor.ThresholdDays != want.ThresholdDays ||
		(cursor.Phase != backlogReconcilePhaseOpen && cursor.Phase != backlogReconcilePhaseClosed) {
		return backlogReconcileCursorState{}, fmt.Errorf("decode backlog reconciliation cursor: integrity mismatch")
	}
	return cursor, nil
}

func newBacklogReconcileCursor(repo providers.RepositoryRef, gaggle, trustLabel string, stalenessPolicy backlogStalenessPolicy, scope backlogReconcileAssigneeScope) backlogReconcileCursorState {
	return backlogReconcileCursorState{
		Schema:         backlogReconcileCursorSchema,
		Gaggle:         gaggle,
		Provider:       string(repo.Provider),
		Repository:     backlogReconcileRepositoryKey(repo),
		TrustLabel:     trustLabel,
		AssignedTo:     scope.assignedTo,
		ScopedAssignee: scope.respectAssignee,
		Ownership:      scope.key(),
		ThresholdDays:  stalenessPolicy.thresholdDays,
		Phase:          backlogReconcilePhaseOpen,
	}
}

func backlogReconcileRepositoryKey(repo providers.RepositoryRef) string {
	if repo.Project != "" {
		return repo.Owner + "/" + repo.Project + "/" + repo.Name
	}
	return repo.Owner + "/" + repo.Name
}

func advanceBacklogReconcileCursor(
	ctx context.Context,
	l instance.Layout,
	key string,
	observed, next backlogReconcileCursorState,
) error {
	store, err := openStageStateStore(l)
	if err != nil {
		return fmt.Errorf("open scheduler state: %w", err)
	}
	return updateJSONState(
		ctx, store, key, claimLockOperationBacklogScanCursor,
		func(value stateclient.Value) (backlogReconcileCursorState, error) {
			return decodeBacklogReconcileCursorValue(value, observed)
		},
		func(cursor backlogReconcileCursorState) ([]byte, error) {
			data, err := json.Marshal(cursor)
			if err != nil {
				return nil, fmt.Errorf("marshal backlog reconciliation cursor: %w", err)
			}
			return data, nil
		},
		func(current backlogReconcileCursorState) (backlogReconcileCursorState, bool, error) {
			if !reflect.DeepEqual(current, observed) {
				return current, false, nil
			}
			return next, true, nil
		},
	)
}

func decodeBacklogReconcileCursorValue(value stateclient.Value, fallback backlogReconcileCursorState) (backlogReconcileCursorState, error) {
	if !value.Exists() {
		return fallback, nil
	}
	var cursor backlogReconcileCursorState
	if err := json.Unmarshal(value.Data, &cursor); err != nil {
		return backlogReconcileCursorState{}, fmt.Errorf("decode backlog reconciliation cursor: %w", err)
	}
	return cursor, nil
}

// reserveOwnedOrphanedClaim decides whether item's goobers:claimed label is an
// orphan this instance may clear, and if so reserves the item and records the
// correction. It returns nil, holding no reservation, when the claim is left
// alone.
//
// Absence from the local ledger only proves that no claim of THIS instance is
// live. Several instances can share one repository, so the provider claim
// epoch behind the label must also be one this instance owns (#5311); a
// foreign epoch is left for its owner, at worst lingering as an orphan.
func reserveOwnedOrphanedClaim(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	itemID string,
	now func() time.Time,
	correction *backlogMetadataCorrection,
	budget *backlogReconcileBudget,
) (*backlogReconcileReservation, error) {
	reservation, acquired, err := reserveBacklogClaimReconciliation(l, repo, itemID, now)
	if err != nil {
		return nil, fmt.Errorf("reserve claim reconciliation for issue #%s: %w", itemID, err)
	}
	if !acquired {
		return nil, nil
	}
	epochRunID, owned, err := ownedProviderClaimEpoch(ctx, l, provider, repo, itemID, budget)
	if err != nil || !owned {
		if releaseErr := releaseBacklogClaimReconciliation(l, *reservation); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release claim-reconciliation reservation: %w", releaseErr))
		}
		if err != nil {
			return nil, fmt.Errorf("inspect provider claim epoch for issue #%s: %w", itemID, err)
		}
		return nil, nil
	}
	correction.orphanedClaim = true
	correction.claimEpochRunID = epochRunID
	correction.removeLabels = append(correction.removeLabels, providers.LabelClaimed)
	correction.reasons = append(correction.reasons,
		"removed `goobers:claimed` because no live claim-ledger lease backs it")
	return reservation, nil
}

// ownedProviderClaimEpoch reports whether every open provider claim epoch on
// the item belongs to this instance, returning the owned epoch's run (empty
// when none is open). An epoch is this instance's when its breadcrumb's
// attribution names this instance's identity or, for an unattributed legacy
// breadcrumb, when the run it names was admitted by this instance root. Any
// epoch by another login is never ours to end. An epoch's age is not
// evidence here: this only ever narrows what reconciliation may remove.
func ownedProviderClaimEpoch(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	itemID string,
	budget *backlogReconcileBudget,
) (string, bool, error) {
	epochs, err := provider.OpenClaimEpochs(ctx, repo, itemID)
	if err != nil {
		return "", false, err
	}
	self := stageInstanceIdentity()
	runID := ""
	for _, epoch := range epochs {
		if !epoch.Trusted {
			return "", false, nil
		}
		if epoch.InstanceID != "" && epoch.InstanceID != self {
			return "", false, nil
		}
		if epoch.InstanceID == "" {
			if _, err := l.FindRunDir(epoch.RunID); err != nil {
				return "", false, nil
			}
		}
		runID = epoch.RunID
	}
	return runID, true, nil
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

func inspectBacklogMetadataWithChildCursor(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	botLogin string,
	now time.Time,
	stalenessPolicy backlogStalenessPolicy,
	recs map[string]blockedRecord,
	childCursor *backlogChildInspectionCursor,
	budgets ...*backlogReconcileBudget,
) (backlogMetadataCorrection, string, error) {
	var budget *backlogReconcileBudget
	if len(budgets) > 0 {
		budget = budgets[0]
	}
	correction := backlogMetadataCorrection{}
	validTracking := false
	if item.HasLabel(providers.LabelClaimed) {
		correction.checkClaim = true
	}
	if item.HasLabel(providers.LabelTracking) {
		hasOpenChildren, _, progress, err := trackingItemHasOpenChildrenBudgeted(ctx, provider, repo, item, budget, childCursor)
		if err != nil {
			return correction, botLogin, fmt.Errorf("inspect tracking children: %w", err)
		}
		correction.childCursor = progress
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
	driftedBlockers, err := driftedBlockedOnSiblingBlockersBudgeted(ctx, provider, repo, item, recs, budget)
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
	infraParkCleared, err := staleInfrastructureRemediationParkBudgeted(ctx, provider, repo, item, budget)
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
		resolved, err := staleBlockedOnSiblingMarkerBudgeted(ctx, provider, repo, item, recs, budget)
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
	budget *backlogReconcileBudget,
	childCursor *backlogChildInspectionCursor,
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
	hasOpenChildren, hasUnverifiedChildren, progress, err := trackingItemHasOpenChildrenBudgeted(ctx, provider, repo, item, budget, childCursor)
	if err != nil {
		return correction, err
	}
	correction.childCursor = progress
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

func trackingItemHasOpenChildrenBudgeted(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	budget *backlogReconcileBudget,
	cursor *backlogChildInspectionCursor,
) (bool, bool, *backlogChildInspectionCursor, error) {
	native, err := provider.ListWorkItemChildren(ctx, repo, item.ID)
	if err != nil {
		return false, false, nil, err
	}
	seen := make(map[string]bool, len(native))
	for _, child := range native {
		seen[child.ID] = true
		if strings.EqualFold(child.State, "open") {
			return true, false, nil, nil
		}
	}
	checklistIDs := trackingChecklistIssueIDs(item.Body)
	fingerprint := trackingChildInspectionFingerprint(native, checklistIDs)
	start := 0
	phase := ""
	if cursor != nil && cursor.ParentID == item.ID && cursor.Fingerprint == fingerprint {
		phase = cursor.Phase
		start = min(cursor.NextIndex, len(checklistIDs))
	}
	hasUnverifiedChildren := false
	for i, id := range checklistIDs {
		if i < start {
			continue
		}
		if seen[id] {
			continue
		}
		child, err := provider.GetWorkItem(ctx, repo, id)
		if err != nil {
			if errors.Is(err, errBacklogReconcileBudgetExhausted) {
				return false, hasUnverifiedChildren, nil, backlogChildInspectionBudgetError{cursor: backlogChildInspectionCursor{
					ParentID:    item.ID,
					Fingerprint: fingerprint,
					NextIndex:   i,
					Phase:       phase,
				}}
			}
			if providers.IsNotFoundError(err) {
				hasUnverifiedChildren = true
				continue
			}
			return false, hasUnverifiedChildren, nil, err
		}
		if strings.EqualFold(child.State, "open") {
			return true, hasUnverifiedChildren, nil, nil
		}
	}
	if start > 0 && phase != backlogChildInspectionPhaseVerify && phase != backlogChildInspectionPhaseDone {
		return false, hasUnverifiedChildren, nil, backlogChildInspectionBudgetError{cursor: backlogChildInspectionCursor{
			ParentID:    item.ID,
			Fingerprint: fingerprint,
			Phase:       backlogChildInspectionPhaseVerify,
		}}
	}
	var progress *backlogChildInspectionCursor
	if phase == backlogChildInspectionPhaseVerify || phase == backlogChildInspectionPhaseDone {
		progress = &backlogChildInspectionCursor{
			ParentID:    item.ID,
			Fingerprint: fingerprint,
			NextIndex:   len(checklistIDs),
			Phase:       backlogChildInspectionPhaseDone,
		}
	}
	return false, hasUnverifiedChildren, progress, nil
}

func trackingChildInspectionFingerprint(native []providers.WorkItem, checklistIDs []string) string {
	hash := sha256.New()
	nativeKeys := make([]string, 0, len(native))
	for _, child := range native {
		nativeKeys = append(nativeKeys, child.ID+"\x00"+child.State)
	}
	sort.Strings(nativeKeys)
	for _, key := range nativeKeys {
		hash.Write([]byte("native\x00"))
		hash.Write([]byte(key))
		hash.Write([]byte{0})
	}
	for _, id := range checklistIDs {
		hash.Write([]byte("checklist\x00"))
		hash.Write([]byte(id))
		hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
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

func staleInfrastructureRemediationParkBudgeted(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	budget *backlogReconcileBudget,
) (bool, error) {
	if !item.HasLabel(needsRemediationLabel) || item.HasLabel(providers.LabelNeedsHuman) {
		return false, nil
	}
	comments, err := provider.ListComments(ctx, repo, item.ID)
	if err != nil {
		return false, err
	}
	return latestParkIsInfrastructure(comments), nil
}

func driftedBlockedOnSiblingBlockersBudgeted(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	recs map[string]blockedRecord,
	budget *backlogReconcileBudget,
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
	return liveLedgerBlockersBudgeted(ctx, provider, repo, recorded, budget)
}

func staleBlockedOnSiblingMarkerBudgeted(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	recs map[string]blockedRecord,
	budget *backlogReconcileBudget,
) (bool, error) {
	if !item.HasLabel(blockedOnSiblingLabel) {
		return false, nil
	}
	recorded := recordedLedgerBlockers(recs, repo, item.ID)
	if len(recorded) > 0 {
		live, err := liveLedgerBlockersBudgeted(ctx, provider, repo, recorded, budget)
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
		return len(recorded) > 0, nil
	}
	live, err := filterLiveBlockedOnSiblingBlockersBudgeted(ctx, provider, repo, state.Blockers, budget)
	if err != nil {
		return false, err
	}
	return len(live) == 0, nil
}

func liveLedgerBlockersBudgeted(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, blockers []string, budget *backlogReconcileBudget) ([]string, error) {
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

func filterLiveBlockedOnSiblingBlockersBudgeted(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, blockers []int, budget *backlogReconcileBudget) ([]int, error) {
	var live []int
	seen := make(map[int]bool)
	for _, blocker := range blockers {
		if seen[blocker] {
			continue
		}
		seen[blocker] = true
		blocks, err := namedBlockerStillBlocksBudgeted(ctx, provider, repo, blocker, budget)
		if err != nil {
			return nil, err
		}
		if blocks {
			live = append(live, blocker)
		}
	}
	return live, nil
}

func namedBlockerStillBlocksBudgeted(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, blocker int, budget *backlogReconcileBudget) (bool, error) {
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
	if !hasAnyLabel(pr.Labels, []string{mergeDemotedLabel}) {
		return true, nil
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return false, err
	}
	state, _, found := latestMergeDemotionState(comments)
	if !found || !state.Demoted {
		return true, nil
	}
	if state.HeadSHA != pr.HeadSHA {
		return false, nil
	}
	return true, nil
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
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != "" {
			out = append(out, label)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
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

// restoreInvisibleClaims re-applies the provider claim marker to items this
// instance holds a live ledger lease on but that show no claim label (#3086).
func restoreInvisibleClaimsWindow(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	observedAt time.Time,
	now func() time.Time,
	stderr io.Writer,
	limit int,
	cursor string,
	scope backlogReconcileAssigneeScope,
) (invisibleClaimScanResult, error) {
	result := invisibleClaimScanResult{Complete: true}
	if limit <= 0 {
		result.Complete = false
		result.NextCursor = cursor
		return result, nil
	}
	budget := newBacklogReconcileBudget(limit, observedAt, now)
	restoreClient := installBacklogReconcileBudget(provider, budget)
	defer restoreClient()
	gaggle := providerGaggle()
	if gaggle == "" {
		pf(stderr, "notice: skipping claim-visibility reconciliation: this stage has no gaggle, so the claim namespace cannot be addressed\n")
		result.Complete = false
		result.NextCursor = cursor
		return result, nil
	}
	ledger, err := openStageClaimLedger(l)
	if err != nil {
		return result, fmt.Errorf("open claim ledger: %w", err)
	}
	listing, err := ledger.ListNamespace(ctx, gaggle, string(repo.Provider))
	if err != nil {
		return result, fmt.Errorf("list claim ledger namespace: %w", err)
	}
	entries := make([]claimsclient.Entry, 0, len(listing.Entries))
	for _, entry := range listing.Entries {
		if entry.ReleasedAt != nil || !entry.ExpiresAt.After(observedAt) {
			continue
		}
		if claimEntryItemID(entry) == "" || entry.RunID == "" {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return claimEntryItemID(entries[i]) < claimEntryItemID(entries[j])
	})
	if len(entries) == 0 {
		return result, nil
	}
	start := 0
	result.NextCursor = cursor
	if cursor != "" {
		start = sort.Search(len(entries), func(i int) bool {
			return claimEntryItemID(entries[i]) > cursor
		})
		if start >= len(entries) {
			start = 0
		}
	}
	incompleteCursor := ""
	for offset := 0; offset < len(entries) && budget.Available(); offset++ {
		entry := entries[(start+offset)%len(entries)]
		itemID := claimEntryItemID(entry)
		result.Examined++
		result.NextCursor = itemID
		entryResult, err := restoreInvisibleClaimWindowEntry(ctx, l, provider, repo, ledger, entry, gaggle, now, budget, stderr, scope)
		result.Spent = budget.Spent()
		if err != nil {
			return result, err
		}
		if entryResult.incomplete && incompleteCursor == "" {
			incompleteCursor = itemID
		}
		if entryResult.restored {
			result.Restored++
		}
	}
	result.Complete = result.Examined >= len(entries) && incompleteCursor == ""
	if !budget.Available() && result.Examined < len(entries) {
		result.Complete = false
	}
	if result.Complete {
		result.NextCursor = ""
	} else if incompleteCursor != "" {
		result.NextCursor = incompleteCursor
	}
	return result, nil
}

type invisibleClaimEntryResult struct {
	restored   bool
	incomplete bool
}

func restoreInvisibleClaimWindowEntry(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	ledger claimsclient.Ledger,
	entry claimsclient.Entry,
	gaggle string,
	now func() time.Time,
	budget *backlogReconcileBudget,
	stderr io.Writer,
	scope backlogReconcileAssigneeScope,
) (invisibleClaimEntryResult, error) {
	itemID := claimEntryItemID(entry)
	if entry.Verification.State == "ownership-mismatch" && !entry.Verification.ObservedAt.IsZero() {
		leaseDuration := entry.ExpiresAt.Sub(entry.ClaimedAt)
		if now().Before(entry.Verification.ObservedAt.Add(leaseDuration)) {
			pf(stderr, "notice: delaying claim-visibility retry for item %s; provider owner %s disagrees with ledger owner %s\n",
				itemID, entry.Verification.ProviderRunID, entry.RunID)
			return invisibleClaimEntryResult{incomplete: true}, nil
		}
	}
	item, err := provider.GetWorkItem(ctx, repo, itemID)
	if err != nil {
		recordClaimObservation(ctx, ledger, entry, localscheduler.ClaimVerification{State: "unavailable", ObservedAt: time.Now()}, stderr)
		pf(stderr, "warning: could not read item %s while checking claim visibility: %v\n", itemID, err)
		return invisibleClaimEntryResult{incomplete: true}, nil
	}
	if item.HasLabel(providers.LabelClaimed) {
		return invisibleClaimEntryResult{}, nil
	}
	if !scope.permits(item) {
		return invisibleClaimEntryResult{}, nil
	}
	live, err := claimEntryStillLive(ctx, ledger, entry, gaggle, string(repo.Provider), now())
	if err != nil {
		pf(stderr, "warning: could not revalidate claim ledger owner for item %s: %v\n", itemID, err)
		return invisibleClaimEntryResult{incomplete: true}, nil
	}
	if !live {
		pf(stderr, "warning: claim visibility for item %s skipped because the ledger lease changed before provider restore\n", itemID)
		return invisibleClaimEntryResult{incomplete: true}, nil
	}
	recordClaimObservation(ctx, ledger, entry, localscheduler.ClaimVerification{State: "missing", ObservedAt: time.Now()}, stderr)
	claim, err := restoreClaimVisibility(ctx, l, provider, repo, item, entry, budget, stderr)
	recordProviderClaimObservation(ctx, ledger, entry, repo, claim, err, stderr)
	if err != nil {
		pf(stderr, "warning: could not restore the claim marker on item %s held by run %s: %v\n",
			itemID, entry.RunID, err)
		return invisibleClaimEntryResult{incomplete: true}, nil
	}
	if !claim.Claimed {
		pf(stderr, "warning: item %s has a live ledger lease held by run %s but the provider claim epoch belongs to run %s\n",
			itemID, entry.RunID, claim.ClaimedBy)
		return invisibleClaimEntryResult{incomplete: true}, nil
	}
	pf(stderr, "notice: restored the claim marker on item %s for its live ledger owner run %s\n", itemID, entry.RunID)
	return invisibleClaimEntryResult{restored: true}, nil
}

func claimEntryStillLive(ctx context.Context, ledger claimsclient.Ledger, entry claimsclient.Entry, gaggle, provider string, now time.Time) (bool, error) {
	wantKey := claimsclient.KeyForEntry(entry)
	live := false
	err := ledger.Locked(ctx, claimLockOperationBacklogReconcile, func(tx claimsclient.Ledger) error {
		listing, err := tx.ListNamespace(ctx, gaggle, provider)
		if err != nil {
			return err
		}
		for _, current := range listing.Entries {
			if claimsclient.KeyForEntry(current) != wantKey || current.RunID != entry.RunID {
				continue
			}
			live = current.ReleasedAt == nil && current.ExpiresAt.After(now)
			return nil
		}
		return nil
	})
	return live, err
}

func claimEntryItemID(entry claimsclient.Entry) string {
	if entry.ExternalID != "" {
		return entry.ExternalID
	}
	return entry.ItemID
}

func advanceBacklogReconcileClaimCursor(
	ctx context.Context,
	l instance.Layout,
	key string,
	nextClaimCursor string,
) error {
	store, err := openStageStateStore(l)
	if err != nil {
		return fmt.Errorf("open scheduler state: %w", err)
	}
	return updateJSONState(
		ctx, store, key, claimLockOperationBacklogScanCursor,
		func(value stateclient.Value) (backlogReconcileCursorState, error) {
			var cursor backlogReconcileCursorState
			if value.Exists() {
				if err := json.Unmarshal(value.Data, &cursor); err != nil {
					return backlogReconcileCursorState{}, fmt.Errorf("decode backlog reconciliation cursor: %w", err)
				}
			}
			return cursor, nil
		},
		func(cursor backlogReconcileCursorState) ([]byte, error) {
			data, err := json.Marshal(cursor)
			if err != nil {
				return nil, fmt.Errorf("marshal backlog reconciliation cursor: %w", err)
			}
			return data, nil
		},
		func(cursor backlogReconcileCursorState) (backlogReconcileCursorState, bool, error) {
			cursor.Claim = nextClaimCursor
			return cursor, true, nil
		},
	)
}

func restoreClaimVisibility(ctx context.Context, l instance.Layout, provider *providers.GitHubProvider, repo providers.RepositoryRef, item providers.WorkItem, entry claimsclient.Entry, budget *backlogReconcileBudget, stderr io.Writer) (providers.ClaimResult, error) {
	if !entry.SharedDeadline.IsZero() {
		labels := budgetedSharedClaimVisibility{
			Visibility: providers.GitHubSharedClaimVisibility{Provider: provider, Repository: repo},
			budget:     budget,
		}
		return confirmSharedClaimVisibility(ctx, entry, stageSharedClaimResolverWithBudget(l, budget), labels, stderr)
	}
	itemID := item.ID
	epochs, err := provider.OpenClaimEpochs(ctx, repo, itemID)
	if err != nil {
		return providers.ClaimResult{}, err
	}
	for _, epoch := range epochs {
		if !epoch.Trusted {
			continue
		}
		if epoch.RunID != entry.RunID {
			return providers.ClaimResult{ClaimedBy: epoch.RunID}, nil
		}
		return restoreLedgerClaimMarker(ctx, provider, repo, itemID, item.Revision, entry.RunID, "", budget)
	}
	return restoreLedgerClaimMarker(ctx, provider, repo, itemID, item.Revision, entry.RunID, "", budget)
}

func restoreLedgerClaimMarker(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, itemID, expectedRevision, runID, comment string, budget *backlogReconcileBudget) (providers.ClaimResult, error) {
	req := providers.UpdateWorkItemRequest{
		Repository:       repo,
		ID:               itemID,
		ExpectedRevision: expectedRevision,
		AddLabels:        []string{providers.LabelClaimed},
		Comment:          comment,
	}
	item, err := updateBacklogWorkItem(ctx, provider, budget, req)
	if err != nil {
		return providers.ClaimResult{}, err
	}
	return providers.ClaimResult{Claimed: true, ClaimedBy: runID, Item: item}, nil
}

type budgetedSharedClaimVisibility struct {
	sharedclaim.Visibility
	budget *backlogReconcileBudget
}

func (v budgetedSharedClaimVisibility) ReadClaimed(ctx context.Context, key string) (bool, error) {
	if err := v.budget.EnsureRemaining(1); err != nil {
		return false, err
	}
	return v.Visibility.ReadClaimed(ctx, key)
}

func (v budgetedSharedClaimVisibility) SetClaimed(ctx context.Context, key string, present bool) error {
	if err := v.budget.EnsureRemaining(1); err != nil {
		return err
	}
	return v.Visibility.SetClaimed(ctx, key, present)
}
