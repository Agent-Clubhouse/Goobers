package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/providers"
)

// blockedRecordsFileName is the well-known file under an instance's
// scheduler/ directory recording learned dependency blocks (#552): items a
// prior run reported blocked on still-open issues, so backlog selection can
// skip them instead of re-spending a full agentic attempt rediscovering the
// identical block every tick. Sibling to claims.json and guarded by the same
// claims.lock — records are written by the runner's blocked handler and
// cleared by backlog-query's self-heal once every recorded blocker closes.
const blockedRecordsFileName = "blocked.json"

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

const maxBlockedCyclePaths = 3

type blockedCycleNode struct {
	Repository providers.RepositoryRef
	ItemID     string
}

type blockedCycleEdge struct {
	From blockedCycleNode
	To   blockedCycleNode
}

type blockedCycleResult struct {
	Affected  []blockedCycleNode
	Paths     [][]string
	MorePaths bool
}

// findBlockedCycle identifies the strongly connected component containing the
// newly recorded item. Forward/reverse reachability is linear in graph size;
// representative shortest paths are capped so dense graphs cannot trigger the
// factorial path enumeration the blocked handler previously performed.
func findBlockedCycle(recs map[string]blockedRecord, itemKey string) blockedCycleResult {
	record, ok := recs[itemKey]
	if !ok || blockedRepositoryEmpty(record.Repository) {
		return blockedCycleResult{}
	}

	keys := make([]string, 0, len(recs))
	for key := range recs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	graph := make(map[blockedCycleNode][]blockedCycleNode, len(recs))
	edgeSeen := make(map[blockedCycleNode]map[blockedCycleNode]bool, len(recs))
	for _, key := range keys {
		rec := recs[key]
		if blockedRepositoryEmpty(rec.Repository) {
			continue
		}
		node := blockedCycleNode{
			Repository: rec.Repository,
			ItemID:     blockedLookupID(blockedRecordItemID(key, rec)),
		}
		if _, ok := graph[node]; !ok {
			graph[node] = nil
		}
		if edgeSeen[node] == nil {
			edgeSeen[node] = make(map[blockedCycleNode]bool)
		}
		for _, blockerID := range rec.Blockers {
			blocker := blockedCycleNode{
				Repository: rec.Repository,
				ItemID:     blockedLookupID(blockerID),
			}
			if !edgeSeen[node][blocker] {
				graph[node] = append(graph[node], blocker)
				edgeSeen[node][blocker] = true
			}
		}
	}

	item := blockedCycleNode{
		Repository: record.Repository,
		ItemID:     blockedLookupID(blockedRecordItemID(itemKey, record)),
	}
	forward := reachableBlockedNodes(graph, item)
	reverseGraph := make(map[blockedCycleNode][]blockedCycleNode, len(graph))
	for from, edges := range graph {
		if _, ok := reverseGraph[from]; !ok {
			reverseGraph[from] = nil
		}
		for _, to := range edges {
			reverseGraph[to] = append(reverseGraph[to], from)
		}
	}
	backward := reachableBlockedNodes(reverseGraph, item)

	component := make(map[blockedCycleNode]bool)
	for node := range forward {
		if backward[node] {
			component[node] = true
		}
	}
	if len(component) == 1 && !slices.Contains(graph[item], item) {
		return blockedCycleResult{}
	}

	affected := []blockedCycleNode{item}
	var others []blockedCycleNode
	for node := range component {
		if node != item {
			others = append(others, node)
		}
	}
	sort.Slice(others, func(i, j int) bool {
		if left, right := blockedRepositoryIdentity(others[i].Repository), blockedRepositoryIdentity(others[j].Repository); left != right {
			return left < right
		}
		return others[i].ItemID < others[j].ItemID
	})
	affected = append(affected, others...)

	var paths [][]string
	coveredNodes := map[blockedCycleNode]bool{item: true}
	coveredEdges := make(map[blockedCycleEdge]bool)
	appendPath := func(nodes []blockedCycleNode) {
		path := make([]string, len(nodes))
		for i, node := range nodes {
			path[i] = node.ItemID
			coveredNodes[node] = true
			if i > 0 {
				coveredEdges[blockedCycleEdge{From: nodes[i-1], To: node}] = true
			}
		}
		paths = append(paths, path)
	}

	if len(component) == 1 {
		appendPath([]blockedCycleNode{item, item})
	} else {
		for _, member := range affected[1:] {
			if coveredNodes[member] || len(paths) == maxBlockedCyclePaths {
				continue
			}
			outbound, found := shortestBlockedPath(graph, item, member, component)
			if !found {
				continue
			}
			inbound, found := shortestBlockedPath(graph, member, item, component)
			if !found {
				continue
			}
			appendPath(append(outbound, inbound[1:]...))
		}
	}

	morePaths := false
	for node := range component {
		if !coveredNodes[node] {
			morePaths = true
			break
		}
	}
	if !morePaths {
		for from, edges := range graph {
			if !component[from] {
				continue
			}
			for _, to := range edges {
				if component[to] && !coveredEdges[blockedCycleEdge{From: from, To: to}] {
					morePaths = true
					break
				}
			}
			if morePaths {
				break
			}
		}
	}
	return blockedCycleResult{Affected: affected, Paths: paths, MorePaths: morePaths}
}

func reachableBlockedNodes(graph map[blockedCycleNode][]blockedCycleNode, start blockedCycleNode) map[blockedCycleNode]bool {
	reached := map[blockedCycleNode]bool{start: true}
	queue := []blockedCycleNode{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range graph[node] {
			if reached[next] {
				continue
			}
			reached[next] = true
			queue = append(queue, next)
		}
	}
	return reached
}

func shortestBlockedPath(graph map[blockedCycleNode][]blockedCycleNode, start, target blockedCycleNode, allowed map[blockedCycleNode]bool) ([]blockedCycleNode, bool) {
	if start == target {
		return []blockedCycleNode{target}, true
	}
	seen := map[blockedCycleNode]bool{start: true}
	previous := make(map[blockedCycleNode]blockedCycleNode)
	queue := []blockedCycleNode{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range graph[node] {
			if !allowed[next] || seen[next] {
				continue
			}
			seen[next] = true
			previous[next] = node
			if next == target {
				path := []blockedCycleNode{target}
				for current := target; current != start; {
					current = previous[current]
					path = append(path, current)
				}
				slices.Reverse(path)
				return path, true
			}
			queue = append(queue, next)
		}
	}
	return nil, false
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

func blockedRecordsPath(l instance.Layout) string {
	return filepath.Join(l.SchedulerDir(), blockedRecordsFileName)
}

func blockedRepositoryIdentity(repo providers.RepositoryRef) string {
	if blockedRepositoryEmpty(repo) {
		return ""
	}
	parts := []string{string(repo.Provider), repo.Owner}
	if repo.Project != "" {
		parts = append(parts, repo.Project)
	}
	parts = append(parts, repo.Name)
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
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
	return repo.Provider == "" && repo.Owner == "" && repo.Project == "" && repo.Name == ""
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
func loadBlockedRecords(path string) (map[string]blockedRecord, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]blockedRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	recs := map[string]blockedRecord{}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return recs, nil
}

func saveBlockedRecords(path string, recs map[string]blockedRecord) error {
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal blocked records: %w", err)
	}
	// Write-then-rename for the same torn-write reason the claim ledger's own
	// persistence uses: a crash mid-write must never leave a half-written
	// file that fails every subsequent selection tick's parse.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := durability.ReplaceFile(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

func snapshotBlockedRecords(l instance.Layout) (map[string]blockedRecord, error) {
	var recs map[string]blockedRecord
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBacklogFilterBlocked, func() error {
		var err error
		recs, err = loadBlockedRecords(blockedRecordsPath(l))
		return err
	})
	return recs, err
}

// snapshotBlockedRecordsForRepository migrates records written before
// repository scoping to the repository the old writer always used. The
// migration runs under claims.lock and is persisted before selection sees the
// snapshot, so an upgrade preserves the existing skip/self-heal behavior
// without allowing legacy records to match every repository.
func snapshotBlockedRecordsForRepository(l instance.Layout, repo providers.RepositoryRef) (map[string]blockedRecord, error) {
	var recs map[string]blockedRecord
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBacklogFilterBlocked, func() error {
		var err error
		recs, err = loadBlockedRecords(blockedRecordsPath(l))
		if err != nil {
			return err
		}
		if migrateLegacyBlockedRecords(recs, repo) {
			return saveBlockedRecords(blockedRecordsPath(l), recs)
		}
		return nil
	})
	return recs, err
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
	path string,
	repo providers.RepositoryRef,
	eligible []providers.WorkItem,
	observedRecords, refreshedRecords map[string]blockedRecord,
	verifiedSkips map[string]blockedEligibilitySkip,
) ([]providers.WorkItem, []blockedEligibilitySkip, error) {
	current, err := loadBlockedRecords(path)
	if err != nil {
		return nil, nil, err
	}

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
	if changed {
		if err := saveBlockedRecords(path, current); err != nil {
			return nil, nil, err
		}
	}

	if len(current) == 0 {
		return eligible, nil, nil
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
	filtered := eligible[:0]
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
	return filtered, skipped, nil
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
func filterBlockedEligibility(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, eligible []providers.WorkItem, recs map[string]blockedRecord) (filtered []providers.WorkItem, skipped []blockedEligibilitySkip, changed bool, warnings []string) {
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
	filtered := candidates
	err = withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBacklogFilterBlocked, func() error {
		var reconcileErr error
		filtered, _, reconcileErr = reconcileBlockedEligibilityLocked(
			blockedRecordsPath(l),
			repo,
			filtered,
			observedRecords,
			refreshedRecords,
			verifiedSkips,
		)
		return reconcileErr
	})
	return filtered, err
}

func listParkedDependencies(l instance.Layout) ([]blockedListRecord, error) {
	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		return nil, err
	}
	return blockedListRecords(recs), nil
}

// updateBlockedRecords applies fn to the records map under the instance's
// claim lock (blocked.json shares claims.lock rather than growing a second
// lock file — writers are the same claim-lifecycle actors) and persists the
// result. fn returns false to skip the write (nothing changed).
func updateBlockedRecords(l instance.Layout, fn func(recs map[string]blockedRecord) bool) error {
	path := blockedRecordsPath(l)
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBlockedUpdate, func() error {
		recs, err := loadBlockedRecords(path)
		if err != nil {
			return err
		}
		if !fn(recs) {
			return nil
		}
		return saveBlockedRecords(path, recs)
	})
}
