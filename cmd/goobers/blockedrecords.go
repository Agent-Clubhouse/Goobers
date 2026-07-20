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

type parkedDependency struct {
	ItemID             string
	Blockers           []string
	repositoryIdentity string
}

const maxBlockedCyclePaths = 3

type blockedCycleNode struct {
	Repository providers.RepositoryRef
	ItemID     string
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
	morePaths := false
	for _, blocker := range graph[item] {
		if !component[blocker] {
			continue
		}
		if len(paths) == maxBlockedCyclePaths {
			morePaths = true
			break
		}
		returnPath, found := shortestBlockedPath(graph, blocker, item, component)
		if !found {
			continue
		}
		path := make([]string, 1, len(returnPath)+1)
		path[0] = item.ItemID
		for _, node := range returnPath {
			path = append(path, node.ItemID)
		}
		paths = append(paths, path)
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
	return blockedRepositoryEmpty(rec.Repository) || sameBlockedRepository(rec.Repository, repo)
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
	if err := os.Rename(tmp, path); err != nil {
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

// reconcileBlockedEligibilityLocked applies provider-resolved removals to the
// latest blocked-record state, then excludes every item that is still recorded
// as blocked. An unchanged record whose own provider lookup failed remains
// eligible, preserving per-record degradation; a concurrent replacement does
// not inherit that exception. The caller must hold claims.lock through any
// subsequent claim so a concurrent blocked-record update cannot race the
// eligibility decision.
func reconcileBlockedEligibilityLocked(path string, repo providers.RepositoryRef, eligible []providers.WorkItem, resolved, unresolved map[string]blockedRecord) ([]providers.WorkItem, error) {
	current, err := loadBlockedRecords(path)
	if err != nil {
		return nil, err
	}

	changed := false
	for itemID, observed := range resolved {
		record, ok := current[itemID]
		if !ok || !sameBlockedRecord(record, observed) {
			continue
		}
		delete(current, itemID)
		changed = true
	}
	if changed {
		if err := saveBlockedRecords(path, current); err != nil {
			return nil, err
		}
	}

	if len(current) == 0 {
		return eligible, nil
	}
	blockedCounts := make(map[string]int)
	unresolvedCounts := make(map[string]int)
	for recordKey, record := range current {
		if !blockedRecordAppliesToRepository(record, repo) {
			continue
		}
		itemID := blockedRecordItemID(recordKey, record)
		blockedCounts[itemID]++
		if observed, ok := unresolved[recordKey]; ok && sameBlockedRecord(record, observed) {
			unresolvedCounts[itemID]++
		}
	}
	filtered := eligible[:0]
	for _, item := range eligible {
		if blockedCounts[item.ID] == 0 {
			filtered = append(filtered, item)
			continue
		}
		if unresolvedCounts[item.ID] == blockedCounts[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
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

// filterBlockedEligibility removes from eligible any item with a recorded
// dependency block (#552) whose blockers are not all closed yet, so
// `implementation` skips known-blocked work instead of re-spending a full
// agentic attempt rediscovering the identical block every tick. It also keeps
// blocked.json from accumulating dead weight (QA-1's gate condition):
//
//   - Self-heal: closed blockers are pruned; once every one of a record's
//     blockers is closed, the record is cleared and the item is eligible
//     again — no human involved.
//   - Prune: a record whose OWN item is no longer open (closed by any path —
//     manual close, a downstream workflow, curation) is cleared outright,
//     since there is nothing left to skip or heal.
//
// GetWorkItem calls are memoized per call (issue ids repeat across records/
// blockers) and scoped to just the recorded items/blockers — a small,
// bounded set proportional to how many items are CURRENTLY blocked, never to
// backlog size. recs is mutated in place; changed reports whether the caller
// must persist it.
// It returns warnings rather than an error: no per-record lookup failure is
// fatal here (see the degradation note below), so an error return would only
// ever be nil. The caller surfaces warnings on stderr so a malformed record
// stays visible instead of silently degrading selection forever. unresolved
// identifies records whose own item lookup failed so the locked reconciliation
// can preserve that per-record degradation if the record is still unchanged.
func filterBlockedEligibility(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, eligible []providers.WorkItem, recs map[string]blockedRecord) (filtered []providers.WorkItem, changed bool, warnings []string, unresolved map[string]blockedRecord) {
	if len(recs) == 0 {
		return eligible, false, nil, nil
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

	// A lookup that cannot be resolved must not fail the whole stage. This
	// function runs on every query-backlog tick, and query-backlog is the
	// shared first stage of implementation and backlog-curation, so returning
	// an error here halts all backlog progress over a single unresolvable key
	// — which is exactly how one malformed "pr/"-prefixed record took the
	// self-hosting loop down for ~45 minutes (#971). The blocked file is a
	// selection OPTIMIZATION (skip work a prior run already proved blocked),
	// never a correctness gate: the worst case from ignoring one bad record is
	// that one item gets re-attempted and re-discovers its own block, which is
	// strictly better than starving every workflow. So degrade per-record and
	// keep going.
	//
	// Conservative in both directions: an unresolvable record is neither
	// pruned (we cannot prove its item closed) nor treated as skippable (we
	// cannot prove it still blocked), so it survives untouched for a later
	// tick — or for a human reading the file — to resolve.
	skip := make(map[string]bool, len(recs))
	var lookupErrs []string
	for recordKey, rec := range recs {
		if !blockedRecordAppliesToRepository(rec, repo) {
			continue
		}
		itemID := blockedRecordItemID(recordKey, rec)
		open, oerr := isOpen(itemID)
		if oerr != nil {
			lookupErrs = append(lookupErrs, fmt.Sprintf("check blocked item %s: %v", itemID, oerr))
			if unresolved == nil {
				unresolved = make(map[string]blockedRecord)
			}
			unresolved[recordKey] = rec
			continue
		}
		if !open {
			delete(recs, recordKey)
			changed = true
			continue
		}

		unresolved := false
		openBlockers := make([]string, 0, len(rec.Blockers))
		for _, blockerID := range rec.Blockers {
			blockerOpen, berr := isOpen(blockerID)
			if berr != nil {
				// Same degradation as above, one level down. An unresolvable
				// blocker must not self-heal the record: "we could not check"
				// is not "it closed", and treating it as closed would silently
				// unblock an item whose dependency may still be open.
				lookupErrs = append(lookupErrs, fmt.Sprintf("check blocker %s for %s: %v", blockerID, itemID, berr))
				unresolved = true
				break
			}
			if blockerOpen {
				openBlockers = append(openBlockers, blockerID)
			}
		}
		if unresolved {
			skip[itemID] = true
			continue
		}
		if len(openBlockers) == 0 {
			delete(recs, recordKey)
			changed = true
			continue
		}
		if len(openBlockers) != len(rec.Blockers) {
			rec.Blockers = openBlockers
			recs[recordKey] = rec
			changed = true
		}
		skip[itemID] = true
	}

	if len(skip) == 0 {
		return eligible, changed, lookupErrs, unresolved
	}
	out := eligible[:0]
	for _, item := range eligible {
		if skip[item.ID] {
			continue
		}
		out = append(out, item)
	}
	return out, changed, lookupErrs, unresolved
}

func refreshBlockedEligibility(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	eligible []providers.WorkItem,
) ([]providers.WorkItem, error) {
	filtered := eligible
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationBacklogFilterBlocked, func() error {
		recs, err := loadBlockedRecords(blockedRecordsPath(l))
		if err != nil {
			return err
		}
		var changed bool
		filtered, changed, _, _ = filterBlockedEligibility(ctx, provider, repo, eligible, recs)
		if !changed {
			return nil
		}
		return saveBlockedRecords(blockedRecordsPath(l), recs)
	})
	return filtered, err
}

func listParkedDependencies(l instance.Layout) ([]parkedDependency, error) {
	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		return nil, err
	}
	parked := make([]parkedDependency, 0, len(recs))
	for recordKey, rec := range recs {
		blockers := append([]string(nil), rec.Blockers...)
		sort.Strings(blockers)
		parked = append(parked, parkedDependency{
			ItemID:             blockedRecordItemID(recordKey, rec),
			Blockers:           blockers,
			repositoryIdentity: blockedRepositoryIdentity(rec.Repository),
		})
	}
	sort.Slice(parked, func(i, j int) bool {
		if parked[i].ItemID != parked[j].ItemID {
			return parked[i].ItemID < parked[j].ItemID
		}
		return parked[i].repositoryIdentity < parked[j].repositoryIdentity
	})
	return parked, nil
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
