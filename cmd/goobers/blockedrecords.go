package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// blockedRecordsFileName is the well-known file under an instance's
// scheduler/ directory recording learned dependency blocks (#552): items a
// prior run reported blocked on still-open issues, so backlog selection can
// skip them instead of re-spending a full agentic attempt rediscovering the
// identical block every tick. Sibling to claims.json and guarded by the same
// claims.lock — records are written by the runner's blocked handler and
// cleared by backlog-query's self-heal once every recorded blocker closes.
//
// Since #3878 every access goes through a stateclient.Store rather than this
// path directly, so a stage running in a pod reaches the daemon's copy over
// the scheduler-state plane under that same claims.lock instead of writing a
// pod-local file nothing else ever reads. The name is the plane's key
// (stateclient.KeyBlockedRecords) as well as the file's name.
const blockedRecordsFileName = stateclient.KeyBlockedRecords

// blockedRecord is one learned dependency block: the issue numbers the item
// was reported blocked on, plus provenance for a human inspecting the file.
type blockedRecord struct {
	Repository providers.RepositoryRef `json:"repository"`
	ItemID     string                  `json:"itemId"`
	Blockers   []string                `json:"blockers"`
	RunID      string                  `json:"runId"`
	Stage      string                  `json:"stage,omitempty"`
	Reason     string                  `json:"reason,omitempty"`
	RecordedAt time.Time               `json:"recordedAt"`
}

// blockedCycleNode and blockedCycleResult are the cycle detector's own types
// (internal/blockedcycle), aliased here so this package's blocked-record
// plumbing keeps reading in its own vocabulary.
type blockedCycleNode = blockedcycle.Node

type blockedCycleResult = blockedcycle.Result

const maxBlockedCyclePaths = blockedcycle.MaxPaths

// blockedCycleRecords projects the recorded blocks onto the detector's input:
// repository-scoped, provider-lookup-normalized ids in deterministic key
// order, skipping records that carry no repository scoping.
func blockedCycleRecords(recs map[string]blockedRecord) []blockedcycle.Record {
	keys := make([]string, 0, len(recs))
	for key := range recs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	records := make([]blockedcycle.Record, 0, len(keys))
	for _, key := range keys {
		rec := recs[key]
		if blockedRepositoryEmpty(rec.Repository) {
			continue
		}
		blockers := make([]string, len(rec.Blockers))
		for i, blockerID := range rec.Blockers {
			blockers[i] = blockedLookupID(blockerID)
		}
		records = append(records, blockedcycle.Record{
			Repository: rec.Repository,
			ItemID:     blockedLookupID(blockedRecordItemID(key, rec)),
			Blockers:   blockers,
		})
	}
	return records
}

// findBlockedCycle identifies the cycle containing the newly recorded item.
func findBlockedCycle(recs map[string]blockedRecord, itemKey string) blockedCycleResult {
	record, ok := recs[itemKey]
	if !ok || blockedRepositoryEmpty(record.Repository) {
		return blockedCycleResult{}
	}
	item := blockedCycleNode{
		Repository: record.Repository,
		ItemID:     blockedLookupID(blockedRecordItemID(itemKey, record)),
	}
	return blockedcycle.Find(blockedCycleRecords(recs), item)
}

// findAllBlockedCycles enumerates every cycle currently present in recs, not
// just the one touching a single just-written key (#1405).
func findAllBlockedCycles(recs map[string]blockedRecord) []blockedCycleResult {
	return blockedcycle.FindAll(blockedCycleRecords(recs))
}

// reconcileBlockedCycleLabels is the ongoing, tick-driven half of the
// blocked-cycle escalation guarantee (#1405).
//
// buildBlockedHandler already escalates every member of a cycle correctly at
// the instant a new record closes it (verified for 2- and 3-member cycles in
// every write order — see TestFindBlockedCycle/TestBuildBlockedHandler in
// this package). The gap is afterward: once a cycle member is fully
// skip-parked, backlog-query's own eligibility filter (#552) never selects it
// again, so its blocked-handler never re-fires — nothing re-checks whether
// its escalation still holds. If anything later resets its labels (a human
// override, a stale re-curation pass) without ever touching blocked.json,
// the item can silently look claimable again while its cycle sibling still
// carries needs-human, exactly the asymmetry #1405 reports.
//
// This closes that gap from the one place that already runs on every
// backlog-query tick regardless of claim state: right after
// filterBlockedEligibility. For every currently active cycle, any member
// whose live labels have drifted off needs-human gets the same escalation
// buildBlockedHandler applies (needs-human added, ready/claimed removed, the
// cycle comment posted) — but only members that need it, so an
// already-escalated cycle is a single read per member and no writes.
//
// Best-effort per member, like filterBlockedEligibility: one item's provider
// read/write failure is reported as a warning and does not stop reconciling
// the rest of the cycle or the next one.
func reconcileBlockedCycleLabels(
	ctx context.Context,
	provider backlogIssueProvider,
	recs map[string]blockedRecord,
	needsHumanAssignee string,
) []string {
	cycles := findAllBlockedCycles(recs)
	if len(cycles) == 0 {
		return nil
	}
	var warnings []string
	for _, cycle := range cycles {
		var comments []string
		for _, member := range cycle.Affected {
			item, err := provider.GetWorkItem(ctx, member.Repository, blockedLookupID(member.ItemID))
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("check cycle member %s#%s: %v", member.Repository.Name, member.ItemID, err))
				continue
			}
			if slices.Contains(item.Labels, providers.LabelNeedsHuman) {
				continue
			}
			// Computed lazily and once per cycle: most ticks find every member
			// already escalated and never need it.
			if comments == nil {
				comments = blockedcycle.Comments(cycle)
			}
			for _, comment := range comments {
				req := withNeedsHumanAssignee(providers.UpdateWorkItemRequest{
					Repository:   member.Repository,
					ID:           member.ItemID,
					Comment:      comment,
					AddLabels:    []string{providers.LabelNeedsHuman},
					RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
				}, needsHumanAssignee)
				if _, err := provider.UpdateWorkItem(ctx, req); err != nil {
					warnings = append(warnings, fmt.Sprintf(
						"re-escalate circular dependency on %s#%s: %v", member.Repository.Name, member.ItemID, err))
				}
			}
		}
	}
	return warnings
}

type blockedEligibilitySkip struct {
	ItemID              string
	ItemStateUnresolved bool
	OpenBlockers        []string
	UnresolvedBlockers  []string
	VerificationPending bool
	record              blockedRecord
}

func (s blockedEligibilitySkip) reason() string {
	if s.ItemStateUnresolved {
		return fmt.Sprintf("learned block: item %s parked; item state unresolved", s.ItemID)
	}
	if s.VerificationPending {
		return fmt.Sprintf("learned block: item %s parked; blocked record changed during eligibility check", s.ItemID)
	}
	if len(s.UnresolvedBlockers) != 0 {
		return fmt.Sprintf(
			"learned block: item %s parked; blocker state unresolved: %s",
			s.ItemID,
			strings.Join(s.UnresolvedBlockers, ","),
		)
	}
	return fmt.Sprintf("learned block: item %s parked on open blocker(s): %s", s.ItemID, strings.Join(s.OpenBlockers, ","))
}

func blockedRepositoryIdentity(repo providers.RepositoryRef) string {
	return blockedcycle.RepositoryIdentity(repo)
}

func blockedRecordKey(repo providers.RepositoryRef, itemID string) string {
	return blockedRepositoryIdentity(repo) + "#" + url.PathEscape(itemID)
}

func blockedRecordItemID(key string, rec blockedRecord) string {
	if rec.ItemID != "" {
		return rec.ItemID
	}
	return key
}

func blockedRepositoryEmpty(repo providers.RepositoryRef) bool {
	return blockedcycle.RepositoryEmpty(repo)
}

func sameBlockedRepository(a, b providers.RepositoryRef) bool {
	return a.Provider == b.Provider && a.Owner == b.Owner && a.Project == b.Project && a.Name == b.Name
}

func blockedRecordAppliesToRepository(rec blockedRecord, repo providers.RepositoryRef) bool {
	// The provider-backed selection path migrates legacy records before calling
	// this helper. Keep any remaining unscoped record quarantined rather than
	// applying it to every repository.
	return !blockedRepositoryEmpty(rec.Repository) && sameBlockedRepository(rec.Repository, repo)
}

// blockedLookupID converts a recorded item id into the id a provider lookup
// expects. Item ids come from whatever the claim ledger used for the run's
// driving item, so a pr-remediation run records its claim name — "pr/955" —
// while issue-driven runs record a bare "955".
//
// GetWorkItem builds its URL as .../issues/{id} literally, so a "pr/"-
// prefixed id produced .../issues/pr/955: an invalid path, a 404, and (before
// this was handled) a hard failure of every query-backlog tick — which took
// down every workflow whose first stage is query-backlog, implementation and
// backlog-curation alike (#971). Stripping the prefix is correct rather than
// merely expedient: GitHub numbers issues and pull requests in one shared
// sequence and serves both at /issues/{number}, so the bare number resolves
// the pull request, and its state drives the same self-heal/prune logic every
// other record gets.
func blockedLookupID(key string) string {
	return strings.TrimPrefix(key, pullRequestClaimPrefix)
}

// loadBlockedRecords reads the records map; a missing file is an empty map
// (the overwhelmingly common steady state), never an error.
// needsHumanAssigneeFor reads the configured needs-human routing assignee
// (mirrors issuecloseout.go's own load) for reconcileBlockedCycleLabels,
// which runs from a provider-stage CLI command with no instance.Config
// already in scope.
func needsHumanAssigneeFor(l instance.Layout) (string, error) {
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return "", fmt.Errorf("load needs-human routing config: %w", err)
	}
	return cfg.NeedsHumanAssignee, nil
}

// decodeBlockedRecords is loadBlockedRecords over a scheduler-state value
// rather than a path: an absent key is the empty map for the same reason a
// missing file is (the overwhelmingly common steady state), and the decode is
// identical whether the bytes arrived from the instance's own file or from the
// scheduler-state plane.
func decodeBlockedRecords(value stateclient.Value) (map[string]blockedRecord, error) {
	recs := map[string]blockedRecord{}
	if !value.Exists() {
		return recs, nil
	}
	if err := json.Unmarshal(value.Data, &recs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", stateclient.KeyBlockedRecords, err)
	}
	return recs, nil
}

// encodeBlockedRecords renders the records map exactly as saveBlockedRecords
// renders it, so an instance that switches between the file backend and the
// plane produces byte-identical blocked.json and its ETag is stable across the
// two paths.
func encodeBlockedRecords(recs map[string]blockedRecord) ([]byte, error) {
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal blocked records: %w", err)
	}
	return data, nil
}

func snapshotBlockedRecords(l instance.Layout) (map[string]blockedRecord, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return nil, err
	}
	value, err := store.Get(stateContext(), stateclient.KeyBlockedRecords)
	if err != nil {
		return nil, err
	}
	return decodeBlockedRecords(value)
}

// snapshotBlockedRecordsForRepository migrates records written before
// repository scoping to the repository the old writer always used. The
// migration runs under claims.lock and is persisted before selection sees the
// snapshot, so an upgrade preserves the existing skip/self-heal behavior
// without allowing legacy records to match every repository.
func snapshotBlockedRecordsForRepository(l instance.Layout, repo providers.RepositoryRef) (map[string]blockedRecord, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return nil, err
	}
	var recs map[string]blockedRecord
	err = store.Update(stateContext(), stateclient.KeyBlockedRecords, claimLockOperationBacklogFilterBlocked,
		func(value stateclient.Value) ([]byte, bool, error) {
			// Recomputed from the observed value on every compare-and-swap
			// attempt: the migration is a pure function of what is currently
			// stored, so a retry after a lost swap migrates the winner's map
			// rather than re-applying a stale one.
			current, decodeErr := decodeBlockedRecords(value)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
			recs = current
			changed := migrateLegacyBlockedRecords(current, repo)
			if repairMalformedBlockedRecordItemIDs(current) {
				changed = true
			}
			if !changed {
				return nil, false, nil
			}
			data, encodeErr := encodeBlockedRecords(current)
			if encodeErr != nil {
				return nil, false, encodeErr
			}
			return data, true, nil
		})
	if err != nil {
		return nil, err
	}
	return recs, nil
}

func migrateLegacyBlockedRecords(recs map[string]blockedRecord, repo providers.RepositoryRef) bool {
	if blockedRepositoryEmpty(repo) {
		return false
	}
	keys := make([]string, 0, len(recs))
	for key := range recs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	changed := false
	for _, key := range keys {
		rec := recs[key]
		if !blockedRepositoryEmpty(rec.Repository) {
			continue
		}
		rec.Repository = repo
		rec.ItemID = blockedRecordItemID(key, rec)
		scopedKey := blockedRecordKey(repo, rec.ItemID)
		if _, exists := recs[scopedKey]; !exists {
			recs[scopedKey] = rec
		}
		delete(recs, key)
		changed = true
	}
	return changed
}

func repairMalformedBlockedRecordItemIDs(recs map[string]blockedRecord) bool {
	changed := false
	for key, rec := range recs {
		if blockedRepositoryEmpty(rec.Repository) || rec.ItemID != key {
			continue
		}
		prefix := blockedRepositoryIdentity(rec.Repository) + "#"
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		itemID, err := url.PathUnescape(strings.TrimPrefix(key, prefix))
		if err != nil || itemID == "" {
			continue
		}
		rec.ItemID = itemID
		recs[key] = rec
		changed = true
	}
	return changed
}

// reconcileBlockedEligibilityLocked applies provider-refreshed records to the
// latest repository-scoped blocked-record state, then excludes every item that
// is still recorded as blocked for this repository. It fails closed: a record
// that changed since it was observed (a concurrent addition or replacement)
// stays parked because its blocker state has not been verified this cycle, and
// a record whose own item or blocker lookups could not be resolved stays parked
// via its recorded skip rather than being released. The caller must hold
// claims.lock through any subsequent claim so a blocked-record update cannot
// race the eligibility decision.
func reconcileBlockedEligibilityLocked(
	ctx context.Context,
	store stateclient.Store,
	repo providers.RepositoryRef,
	eligible []providers.WorkItem,
	observedRecords, refreshedRecords map[string]blockedRecord,
	verifiedSkips map[string]blockedEligibilitySkip,
) ([]providers.WorkItem, []blockedEligibilitySkip, error) {
	var (
		filtered []providers.WorkItem
		skipped  []blockedEligibilitySkip
	)
	// One read-modify-write. On the file backend that is the single locked
	// section it has always been; on the scheduler-state plane it is a
	// compare-and-swap the daemon serves under that same lock, retried on a
	// lost swap. The body is therefore written to be re-runnable: every
	// decision is recomputed from the value it just observed, and nothing is
	// accumulated across attempts.
	err := store.Update(ctx, stateclient.KeyBlockedRecords, claimLockOperationBacklogFilterBlocked,
		func(value stateclient.Value) ([]byte, bool, error) {
			current, decodeErr := decodeBlockedRecords(value)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
			changed := applyRefreshedBlockedRecords(current, observedRecords, refreshedRecords, verifiedSkips)
			filtered, skipped = partitionBlockedEligibility(current, repo, eligible, verifiedSkips)
			if !changed {
				return nil, false, nil
			}
			data, encodeErr := encodeBlockedRecords(current)
			if encodeErr != nil {
				return nil, false, encodeErr
			}
			return data, true, nil
		})
	if err != nil {
		return nil, nil, err
	}
	return filtered, skipped, nil
}

// applyRefreshedBlockedRecords folds this cycle's provider-refreshed records
// into current, reporting whether current changed. A record that no longer
// matches what was observed is left alone — the concurrent writer's version
// wins and the item fails closed downstream.
func applyRefreshedBlockedRecords(
	current, observedRecords, refreshedRecords map[string]blockedRecord,
	verifiedSkips map[string]blockedEligibilitySkip,
) bool {
	changed := false
	for recordKey, observed := range observedRecords {
		record, ok := current[recordKey]
		if !ok || !sameBlockedRecord(record, observed) {
			continue
		}
		refreshed, remains := refreshedRecords[recordKey]
		if !remains {
			delete(current, recordKey)
			changed = true
			continue
		}
		if sameBlockedRecord(record, refreshed) {
			continue
		}
		current[recordKey] = refreshed
		changed = true
		itemID := blockedRecordItemID(recordKey, observed)
		if skip, ok := verifiedSkips[itemID]; ok && sameBlockedRecord(skip.record, observed) {
			skip.record = refreshed
			verifiedSkips[itemID] = skip
		}
	}
	return changed
}

// partitionBlockedEligibility splits eligible into the items selection may
// take and the items that stay parked, given the blocked records current for
// this repository. It is a pure function of its inputs so a compare-and-swap
// retry recomputes it against the value that actually won.
func partitionBlockedEligibility(
	current map[string]blockedRecord,
	repo providers.RepositoryRef,
	eligible []providers.WorkItem,
	verifiedSkips map[string]blockedEligibilitySkip,
) ([]providers.WorkItem, []blockedEligibilitySkip) {
	if len(current) == 0 {
		return eligible, nil
	}
	// After migration every record that applies to this repository has a
	// distinct item id, so a per-item map is a faithful 1:1 view of the
	// repository-scoped block state.
	applicable := make(map[string]blockedRecord, len(current))
	for recordKey, record := range current {
		if !blockedRecordAppliesToRepository(record, repo) {
			continue
		}
		applicable[blockedRecordItemID(recordKey, record)] = record
	}
	// A fresh slice, not eligible[:0]: the caller's slice must survive intact
	// for a compare-and-swap retry to partition it again.
	filtered := make([]providers.WorkItem, 0, len(eligible))
	var skipped []blockedEligibilitySkip
	for _, item := range eligible {
		record, blocked := applicable[item.ID]
		if !blocked {
			filtered = append(filtered, item)
			continue
		}
		// Only release on a verified skip whose record still matches what we
		// checked. Any other state (a concurrently changed record) fails closed:
		// the item stays parked until the next cycle re-verifies it.
		if skip, ok := verifiedSkips[item.ID]; ok && sameBlockedRecord(record, skip.record) {
			skipped = append(skipped, skip)
			continue
		}
		skipped = append(skipped, blockedEligibilitySkip{
			ItemID:              item.ID,
			VerificationPending: true,
			record:              record,
		})
	}
	return filtered, skipped
}

func sameBlockedRecord(a, b blockedRecord) bool {
	return sameBlockedRepository(a.Repository, b.Repository) &&
		a.ItemID == b.ItemID &&
		slices.Equal(a.Blockers, b.Blockers) &&
		a.RunID == b.RunID &&
		a.Stage == b.Stage &&
		a.Reason == b.Reason &&
		a.RecordedAt.Equal(b.RecordedAt)
}

// filterBlockedEligibility refreshes each recorded dependency block (#552) for
// the current repository against live provider state and reports which eligible
// items must stay parked, so `implementation` skips known-blocked work instead
// of re-spending a full agentic attempt rediscovering the identical block every
// tick. It also keeps blocked.json from accumulating dead weight (QA-1's gate
// condition):
//
//   - Self-heal: closed blockers are pruned; once every one of a record's
//     blockers is verified closed, the record is cleared and the item is
//     eligible again — no human involved.
//   - Prune: a record whose OWN item is no longer open (closed by any path —
//     manual close, a downstream workflow, curation) is cleared outright,
//     since there is nothing left to skip or heal.
//
// It fails closed on every unresolved provider lookup (#792): an item whose own
// state cannot be resolved, or a record with any blocker whose state cannot be
// resolved, is reported as a skip and stays parked rather than being released
// or pruned — "we could not check" is never treated as "it closed". Records are
// keyed and scoped by repository (recordKey), so a record belonging to another
// repository is ignored here. GetWorkItem calls are memoized per call (issue ids
// repeat across records/blockers) and scoped to just the recorded items/blockers
// — a small, bounded set proportional to how many items are CURRENTLY blocked,
// never to backlog size. recs is mutated in place; changed reports whether the
// caller must persist it. It returns warnings rather than an error so a single
// unresolvable record never stalls every backlog tick (#971); the caller
// surfaces them on stderr.
func filterBlockedEligibility(ctx context.Context, provider backlogIssueProvider, repo providers.RepositoryRef, eligible []providers.WorkItem, recs map[string]blockedRecord) (filtered []providers.WorkItem, skipped []blockedEligibilitySkip, changed bool, warnings []string) {
	if len(recs) == 0 {
		return eligible, nil, false, nil
	}

	openCache := map[string]bool{}
	isOpen := func(id string) (bool, error) {
		if v, ok := openCache[id]; ok {
			return v, nil
		}
		item, gerr := provider.GetWorkItem(ctx, repo, blockedLookupID(id))
		if gerr != nil {
			return false, gerr
		}
		open := strings.EqualFold(item.State, "open")
		openCache[id] = open
		return open, nil
	}

	recordKeys := make([]string, 0, len(recs))
	for recordKey := range recs {
		recordKeys = append(recordKeys, recordKey)
	}
	sort.Strings(recordKeys)

	eligibleIDs := make(map[string]bool, len(eligible))
	for _, item := range eligible {
		eligibleIDs[item.ID] = true
	}

	skip := make(map[string]bool, len(recs))
	var remove []string
	var lookupWarnings []string
	for _, recordKey := range recordKeys {
		rec := recs[recordKey]
		if !blockedRecordAppliesToRepository(rec, repo) {
			continue
		}
		itemID := blockedRecordItemID(recordKey, rec)
		open, oerr := isOpen(itemID)
		if oerr != nil {
			// Fail closed: an item whose own state cannot be resolved stays
			// parked, never pruned and never released.
			lookupWarnings = append(lookupWarnings, fmt.Sprintf("check blocked item %s: %v", itemID, oerr))
			if eligibleIDs[itemID] {
				skip[itemID] = true
				skipped = append(skipped, blockedEligibilitySkip{
					ItemID:              itemID,
					ItemStateUnresolved: true,
					record:              rec,
				})
			}
			continue
		}
		if !open {
			remove = append(remove, recordKey)
			continue
		}

		blockerIDs := append([]string(nil), rec.Blockers...)
		sort.Strings(blockerIDs)
		var openBlockers []string
		var unresolvedBlockers []string
		for _, blockerID := range blockerIDs {
			blockerOpen, berr := isOpen(blockerID)
			if berr != nil {
				// Same fail-closed rule one level down: an unresolvable blocker
				// must not self-heal the record.
				lookupWarnings = append(lookupWarnings, fmt.Sprintf("check blocker %s for %s: %v", blockerID, itemID, berr))
				unresolvedBlockers = append(unresolvedBlockers, blockerID)
				continue
			}
			if blockerOpen {
				openBlockers = append(openBlockers, blockerID)
			}
		}
		if len(unresolvedBlockers) != 0 {
			if eligibleIDs[itemID] {
				skip[itemID] = true
				skipped = append(skipped, blockedEligibilitySkip{
					ItemID:             itemID,
					OpenBlockers:       openBlockers,
					UnresolvedBlockers: unresolvedBlockers,
					record:             rec,
				})
			}
			refreshedBlockers := append(append([]string(nil), openBlockers...), unresolvedBlockers...)
			sort.Strings(refreshedBlockers)
			if !slices.Equal(rec.Blockers, refreshedBlockers) {
				rec.Blockers = refreshedBlockers
				recs[recordKey] = rec
				changed = true
			}
			continue
		}
		if len(openBlockers) == 0 {
			remove = append(remove, recordKey)
			continue
		}
		if eligibleIDs[itemID] {
			skip[itemID] = true
			skipped = append(skipped, blockedEligibilitySkip{ItemID: itemID, OpenBlockers: openBlockers, record: rec})
		}
		if !slices.Equal(rec.Blockers, openBlockers) {
			rec.Blockers = openBlockers
			recs[recordKey] = rec
			changed = true
		}
	}

	for _, recordKey := range remove {
		delete(recs, recordKey)
		changed = true
	}

	if len(skip) == 0 {
		return eligible, skipped, changed, lookupWarnings
	}
	out := eligible[:0]
	for _, item := range eligible {
		if skip[item.ID] {
			continue
		}
		out = append(out, item)
	}
	return out, skipped, changed, lookupWarnings
}

func refreshBlockedEligibility(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	eligible []providers.WorkItem,
) ([]providers.WorkItem, error) {
	observedRecords, err := snapshotBlockedRecordsForRepository(l, repo)
	if err != nil {
		return nil, err
	}
	refreshedRecords := make(map[string]blockedRecord, len(observedRecords))
	for recordKey, record := range observedRecords {
		refreshedRecords[recordKey] = record
	}
	candidates := append([]providers.WorkItem(nil), eligible...)
	_, observedSkips, _, _ := filterBlockedEligibility(ctx, provider, repo, append([]providers.WorkItem(nil), candidates...), refreshedRecords)
	verifiedSkips := make(map[string]blockedEligibilitySkip, len(observedSkips))
	for _, skip := range observedSkips {
		verifiedSkips[skip.ItemID] = skip
	}
	store, err := openStageStateStore(l)
	if err != nil {
		return nil, err
	}
	filtered, _, err := reconcileBlockedEligibilityLocked(
		ctx,
		store,
		repo,
		candidates,
		observedRecords,
		refreshedRecords,
		verifiedSkips,
	)
	if err != nil {
		return nil, err
	}
	return filtered, nil
}

// updateBlockedRecords applies fn to the records map under the instance's
// claim lock (blocked.json shares claims.lock rather than growing a second
// lock file — writers are the same claim-lifecycle actors) and persists the
// result. fn returns false to skip the write (nothing changed).
func updateBlockedRecords(l instance.Layout, fn func(recs map[string]blockedRecord) bool) error {
	store, err := openStageStateStore(l)
	if err != nil {
		return err
	}
	return store.Update(stateContext(), stateclient.KeyBlockedRecords, claimLockOperationBlockedUpdate,
		func(value stateclient.Value) ([]byte, bool, error) {
			// fn runs against the value observed on THIS attempt, so a lost
			// compare-and-swap re-applies the caller's mutation to the winner's
			// map rather than overwriting it — the lost-update this route
			// exists to prevent.
			recs, decodeErr := decodeBlockedRecords(value)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
			if !fn(recs) {
				return nil, false, nil
			}
			data, encodeErr := encodeBlockedRecords(recs)
			if encodeErr != nil {
				return nil, false, encodeErr
			}
			return data, true, nil
		})
}
