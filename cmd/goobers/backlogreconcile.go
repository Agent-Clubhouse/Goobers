package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
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
	backlogReconcileCursorSchema      = "goobers.dev/backlog-reconcile-cursor/v1"
	backlogReconcilePhaseOpen         = "open"
	backlogReconcilePhaseClosed       = "closed"
	backlogReconcilePhaseClaims       = "claims"
)

type backlogMetadataCorrection struct {
	addLabels           []string
	removeLabels        []string
	reasons             []string
	checkClaim          bool
	orphanedClaim       bool
	claimEpochRunID     string
	trackingComplete    bool
	closeTrackingParent bool
}

type inspectedBacklogItem struct {
	item       providers.WorkItem
	correction backlogMetadataCorrection
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
	Schema        string            `json:"schema"`
	Gaggle        string            `json:"gaggle,omitempty"`
	Provider      string            `json:"provider"`
	Repository    string            `json:"repository"`
	TrustLabel    string            `json:"trustLabel"`
	ThresholdDays int               `json:"thresholdDays"`
	Phase         string            `json:"phase"`
	Open          backlogScanCursor `json:"open,omitempty"`
	Closed        backlogScanCursor `json:"closed,omitempty"`
	Claim         string            `json:"claim,omitempty"`
}

type backlogReconcileReservation struct {
	itemID   string
	gaggle   string
	provider string
	runID    string
}

func reconcileBacklogMetadataDetailed(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	trustLabel string,
	stalenessPolicy backlogStalenessPolicy,
	now func() time.Time,
) (backlogReconciliationResult, error) {
	result := backlogReconciliationResult{}
	// Reap terminal and expired ledger leases before inspecting provider labels.
	// This makes the ledger's liveness decision available to the provider-marker
	// reconciliation below, so a dead claimant cannot keep its marker forever.
	// Through the seam (staleclaimrecovery.go): the daemon runs the sweep when
	// this stage is pod-dispatched and has no instance root of its own.
	if err := recoverStageClaims(l, now()); err != nil {
		return result, fmt.Errorf("recover stale claims before metadata reconciliation: %w", err)
	}
	observedAt := now()
	items, scan, cursorKey, observedCursor, nextCursor, err := listBacklogItemsForReconciliation(ctx, l, provider, repo, trustLabel, stalenessPolicy, observedAt)
	if err != nil {
		return result, fmt.Errorf("list trusted backlog items: %w", err)
	}
	result.Scan = scan
	result.cursorKey = cursorKey
	result.observedCursor = observedCursor
	result.nextCursor = nextCursor

	blockedRecords, err := snapshotBlockedRecords(l)
	if err != nil {
		return result, fmt.Errorf("snapshot learned block ledger: %w", err)
	}
	botLogin := ""
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
			return result, fmt.Errorf("refresh issue #%s: %w", item.ID, err)
		}
		correction, login, err := inspectBacklogMetadata(ctx, provider, repo, current, botLogin, observedAt, stalenessPolicy, blockedRecords)
		if err != nil {
			return result, fmt.Errorf("inspect issue #%s: %w", item.ID, err)
		}
		botLogin = login
		if !correction.checkClaim && len(correction.removeLabels) == 0 && len(correction.addLabels) == 0 {
			continue
		}
		inspected = append(inspected, inspectedBacklogItem{item: current, correction: correction})
	}

	for _, inspectedItem := range inspected {
		current := inspectedItem.item
		current, err = provider.GetWorkItem(ctx, repo, current.ID)
		if err != nil {
			return result, fmt.Errorf("refresh issue #%s before reconcile: %w", inspectedItem.item.ID, err)
		}
		correction, login, err := inspectBacklogMetadata(ctx, provider, repo, current, botLogin, observedAt, stalenessPolicy, blockedRecords)
		if err != nil {
			return result, fmt.Errorf("reinspect issue #%s before reconcile: %w", current.ID, err)
		}
		botLogin = login
		if !correction.checkClaim && len(correction.removeLabels) == 0 && len(correction.addLabels) == 0 {
			continue
		}
		if correction.trackingComplete {
			correction, err = revalidateCompletedTrackingItem(ctx, provider, repo, current.ID, correction)
			if err != nil {
				return result, fmt.Errorf("revalidate tracking issue #%s: %w", current.ID, err)
			}
		}
		var reservation *backlogReconcileReservation
		if correction.checkClaim {
			reservation, err = reserveOwnedOrphanedClaim(ctx, l, provider, repo, current.ID, now, &correction)
			if err != nil {
				return result, err
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
					correction.claimEpochRunID,
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
		if errors.Is(correctionErr, providers.ErrClaimEpochNotOwned) {
			// A claim this instance does not own appeared after the ownership
			// check; leaving the item untouched is the correct outcome (#5311).
			correctionErr = nil
		} else if correctionErr == nil {
			result.Reconciled++
		}
		if reservation != nil {
			if releaseErr := releaseBacklogClaimReconciliation(l, *reservation); releaseErr != nil {
				correctionErr = errors.Join(correctionErr, fmt.Errorf("release claim-reconciliation reservation: %w", releaseErr))
			}
		}
		if correctionErr != nil {
			return result, fmt.Errorf("reconcile issue #%s: %w", current.ID, correctionErr)
		}
	}
	if err := advanceBacklogReconcileCursor(ctx, l, cursorKey, observedCursor, nextCursor); err != nil {
		return result, fmt.Errorf("advance backlog reconciliation cursor: %w", err)
	}
	return result, nil
}

func listBacklogItemsForReconciliation(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	trustLabel string,
	stalenessPolicy backlogStalenessPolicy,
	observedAt time.Time,
) ([]providers.WorkItem, backlogReconcileScan, string, backlogReconcileCursorState, backlogReconcileCursorState, error) {
	scan := backlogReconcileScan{Budget: backlogReconcileScanBudget()}
	key := backlogReconcileCursorKey(repo, providerGaggle(), trustLabel, stalenessPolicy)
	store, err := openStageStateStore(l)
	if err != nil {
		return nil, scan, key, backlogReconcileCursorState{}, backlogReconcileCursorState{}, fmt.Errorf("open scheduler state: %w", err)
	}
	observed, err := readBacklogReconcileCursor(ctx, store, key, repo, providerGaggle(), trustLabel, stalenessPolicy)
	if err != nil {
		return nil, scan, key, observed, backlogReconcileCursorState{}, err
	}
	next := observed
	next.Phase = backlogReconcilePhaseOpen
	items := make([]providers.WorkItem, 0, scan.Budget)
	metadataBudget := backlogReconcileMetadataBudget(scan.Budget)
	openBudget := (metadataBudget + 1) / 2
	openItems, openWindow, err := listBacklogReconcilePhaseWindow(ctx, provider, repo, trustLabel, backlogReconcilePhaseOpen, stalenessPolicy, observedAt, openBudget, observed.Open)
	if err != nil {
		return nil, scan, key, observed, next, err
	}
	items = append(items, openItems...)
	scan.Examined += openWindow.Examined
	scan.Spent += openWindow.Spent
	scan.OpenExamined += openWindow.Examined
	next.Open = openWindow.Cursor
	remaining := metadataBudget - openWindow.Spent
	if openWindow.Truncated {
		scan.WorkRemaining = true
		scan.NextPhase = backlogReconcilePhaseOpen
		scan.NextCursor = openWindow.Cursor.Cursor
	}
	if remaining > 0 {
		closedItems, closedWindow, err := listBacklogReconcilePhaseWindow(ctx, provider, repo, trustLabel, backlogReconcilePhaseClosed, stalenessPolicy, observedAt, remaining, observed.Closed)
		if err != nil {
			return nil, scan, key, observed, next, err
		}
		items = append(items, closedItems...)
		scan.Examined += closedWindow.Examined
		scan.Spent += closedWindow.Spent
		scan.ClosedExamined += closedWindow.Examined
		next.Closed = closedWindow.Cursor
		if closedWindow.Truncated {
			scan.WorkRemaining = true
			if scan.NextPhase == "" {
				scan.NextPhase = backlogReconcilePhaseClosed
				scan.NextCursor = closedWindow.Cursor.Cursor
			}
		}
	}
	scan.Complete = !scan.WorkRemaining
	return items, scan, key, observed, next, nil
}

func backlogReconcileMetadataBudget(total int) int {
	if total > 1 {
		return total - max(1, total/10)
	}
	return total
}

type backlogReconcilePhaseWindow struct {
	Cursor    backlogScanCursor
	Examined  int
	Spent     int
	Truncated bool
}

func listBacklogReconcilePhaseWindow(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	trustLabel, phase string,
	stalenessPolicy backlogStalenessPolicy,
	observedAt time.Time,
	limit int,
	cursor backlogScanCursor,
) ([]providers.WorkItem, backlogReconcilePhaseWindow, error) {
	window := backlogReconcilePhaseWindow{Cursor: cursor}
	state := phase
	var updatedSince *time.Time
	if phase == backlogReconcilePhaseClosed {
		closedSince := observedAt.Add(-stalenessPolicy.threshold())
		updatedSince = &closedSince
	} else {
		state = backlogReconcilePhaseOpen
	}
	scan := backlogReconcilePhaseScan{items: make([]providers.WorkItem, 0, limit), seen: make(map[string]bool, limit), budget: limit}
	position, exhausted, err := scan.run(ctx, provider, repo, trustLabel, state, updatedSince, cursor, false, "")
	if err != nil {
		return nil, window, err
	}
	wrapped := false
	if exhausted && cursor.Cursor != "" && scan.budget > 0 {
		wrapped = true
		position, exhausted, err = scan.run(ctx, provider, repo, trustLabel, state, updatedSince, backlogScanCursor{}, true, cursor.Cursor)
		if err != nil {
			return nil, window, err
		}
	}
	if exhausted && cursor.Cursor != "" && scan.budget <= 0 && !wrapped {
		position = backlogScanCursor{}
		exhausted = false
	}
	window.Examined = len(scan.items)
	window.Spent = scan.spent
	window.Truncated = !exhausted
	window.Cursor = position
	if exhausted {
		window.Cursor = backlogScanCursor{}
	}
	return scan.items, window, nil
}

type backlogReconcilePhaseScan struct {
	items  []providers.WorkItem
	seen   map[string]bool
	budget int
	spent  int
}

func (s *backlogReconcilePhaseScan) run(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	trustLabel, state string,
	updatedSince *time.Time,
	start backlogScanCursor,
	stopOnOverlap bool,
	stopCursor string,
) (backlogScanCursor, bool, error) {
	cursor := start
	maxPages := (s.budget+backlogScanPageSize-1)/backlogScanPageSize + 1
	for page := 0; page < maxPages; page++ {
		pageLimit := min(backlogScanPageSize, s.budget)
		pageInfo := &providers.ListWorkItemsPageInfo{}
		pageItems, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
			Repository:   repo,
			Labels:       []string{trustLabel},
			State:        state,
			UpdatedSince: updatedSince,
			Limit:        pageLimit,
			Cursor:       cursor.Cursor,
			PageInfo:     pageInfo,
			OldestFirst:  true,
		})
		if err != nil {
			return cursor, false, err
		}
		if pageInfo.CandidateCount < 0 {
			return cursor, false, fmt.Errorf("provider returned invalid work-item candidate count %d", pageInfo.CandidateCount)
		}
		s.spent += pageInfo.CandidateCount
		s.budget -= pageInfo.CandidateCount
		overlapped := false
		for _, item := range pageItems {
			if s.seen[item.ID] {
				overlapped = true
				continue
			}
			s.seen[item.ID] = true
			s.items = append(s.items, item)
		}
		if !pageInfo.HasNext {
			return backlogScanCursor{}, true, nil
		}
		if stopOnOverlap && overlapped {
			return backlogScanCursor{}, true, nil
		}
		if pageInfo.CandidateCount == 0 || pageInfo.NextCursor == "" {
			return cursor, false, fmt.Errorf("provider returned a non-advancing work-item cursor")
		}
		cursor.Cursor = pageInfo.NextCursor
		if stopOnOverlap && backlogReconcileCursorReached(cursor.Cursor, stopCursor) {
			return backlogScanCursor{}, true, nil
		}
		if s.budget <= 0 {
			return cursor, false, nil
		}
	}

	return cursor, false, nil
}

type invisibleClaimScanResult struct {
	Restored   int
	Examined   int
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
	const minBacklogReconcileScanBudget = 3
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

func backlogReconcileCursorKey(repo providers.RepositoryRef, gaggle, trustLabel string, stalenessPolicy backlogStalenessPolicy) string {
	key, _ := json.Marshal(struct {
		Repository    providers.RepositoryRef `json:"repository"`
		Gaggle        string                  `json:"gaggle,omitempty"`
		TrustLabel    string                  `json:"trustLabel"`
		ThresholdDays int                     `json:"thresholdDays"`
	}{
		Repository:    repo,
		Gaggle:        gaggle,
		TrustLabel:    trustLabel,
		ThresholdDays: stalenessPolicy.thresholdDays,
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
) (backlogReconcileCursorState, error) {
	value, err := store.Get(ctx, key)
	if err != nil {
		return backlogReconcileCursorState{}, err
	}
	if !value.Exists() {
		return newBacklogReconcileCursor(repo, gaggle, trustLabel, stalenessPolicy), nil
	}
	var cursor backlogReconcileCursorState
	if err := json.Unmarshal(value.Data, &cursor); err != nil {
		return backlogReconcileCursorState{}, fmt.Errorf("decode backlog reconciliation cursor: %w", err)
	}
	want := newBacklogReconcileCursor(repo, gaggle, trustLabel, stalenessPolicy)
	if cursor.Schema != want.Schema ||
		cursor.Gaggle != want.Gaggle ||
		cursor.Provider != want.Provider ||
		cursor.Repository != want.Repository ||
		cursor.TrustLabel != want.TrustLabel ||
		cursor.ThresholdDays != want.ThresholdDays ||
		(cursor.Phase != backlogReconcilePhaseOpen && cursor.Phase != backlogReconcilePhaseClosed) {
		return backlogReconcileCursorState{}, fmt.Errorf("decode backlog reconciliation cursor: integrity mismatch")
	}
	return cursor, nil
}

func newBacklogReconcileCursor(repo providers.RepositoryRef, gaggle, trustLabel string, stalenessPolicy backlogStalenessPolicy) backlogReconcileCursorState {
	return backlogReconcileCursorState{
		Schema:        backlogReconcileCursorSchema,
		Gaggle:        gaggle,
		Provider:      string(repo.Provider),
		Repository:    backlogReconcileRepositoryKey(repo),
		TrustLabel:    trustLabel,
		ThresholdDays: stalenessPolicy.thresholdDays,
		Phase:         backlogReconcilePhaseOpen,
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
	return store.Update(ctx, key, claimLockOperationBacklogScanCursor,
		func(value stateclient.Value) ([]byte, bool, error) {
			current, err := decodeBacklogReconcileCursorValue(value, observed)
			if err != nil {
				return nil, false, err
			}
			if current != observed {
				return nil, false, nil
			}
			data, err := json.Marshal(next)
			if err != nil {
				return nil, false, fmt.Errorf("marshal backlog reconciliation cursor: %w", err)
			}
			return data, true, nil
		})
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
) (*backlogReconcileReservation, error) {
	reservation, acquired, err := reserveBacklogClaimReconciliation(l, repo, itemID, now)
	if err != nil {
		return nil, fmt.Errorf("reserve claim reconciliation for issue #%s: %w", itemID, err)
	}
	if !acquired {
		return nil, nil
	}
	epochRunID, owned, err := ownedProviderClaimEpoch(ctx, l, provider, repo, itemID)
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
) (invisibleClaimScanResult, error) {
	result := invisibleClaimScanResult{Complete: true}
	if limit <= 0 {
		result.Complete = false
		result.NextCursor = cursor
		return result, nil
	}
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
	if cursor != "" {
		start = sort.Search(len(entries), func(i int) bool {
			return claimEntryItemID(entries[i]) > cursor
		})
		if start >= len(entries) {
			start = 0
		}
	}
	incompleteCursor := ""
	for offset := 0; offset < len(entries) && result.Examined < limit; offset++ {
		entry := entries[(start+offset)%len(entries)]
		itemID := claimEntryItemID(entry)
		result.Examined++
		result.NextCursor = itemID
		entryResult, err := restoreInvisibleClaimWindowEntry(ctx, l, provider, repo, ledger, entry, gaggle, now, stderr)
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
	stderr io.Writer,
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
	claim, err := restoreClaimVisibility(ctx, l, provider, repo, itemID, entry, stderr)
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
	return store.Update(ctx, key, claimLockOperationBacklogScanCursor,
		func(value stateclient.Value) ([]byte, bool, error) {
			var cursor backlogReconcileCursorState
			if value.Exists() {
				if err := json.Unmarshal(value.Data, &cursor); err != nil {
					return nil, false, fmt.Errorf("decode backlog reconciliation cursor: %w", err)
				}
			}
			cursor.Claim = nextClaimCursor
			data, err := json.Marshal(cursor)
			if err != nil {
				return nil, false, fmt.Errorf("marshal backlog reconciliation cursor: %w", err)
			}
			return data, true, nil
		})
}

func restoreClaimVisibility(ctx context.Context, l instance.Layout, provider *providers.GitHubProvider, repo providers.RepositoryRef, itemID string, entry claimsclient.Entry, stderr io.Writer) (providers.ClaimResult, error) {
	if !entry.SharedDeadline.IsZero() {
		labels := providers.GitHubSharedClaimVisibility{Provider: provider, Repository: repo}
		return confirmSharedClaimVisibility(ctx, entry, stageSharedClaimResolver(l), labels, stderr)
	}
	return provider.ClaimWorkItem(ctx, providers.ClaimWorkItemRequest{Repository: repo, ID: itemID, RunID: entry.RunID})
}
