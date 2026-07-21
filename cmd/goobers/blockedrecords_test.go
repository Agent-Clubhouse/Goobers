package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func blockedCycleTestRecords(repo providers.RepositoryRef, dependencies map[string][]string) map[string]blockedRecord {
	recs := make(map[string]blockedRecord, len(dependencies))
	for itemID, blockers := range dependencies {
		recs[blockedRecordKey(repo, itemID)] = blockedRecord{
			Repository: repo,
			ItemID:     itemID,
			Blockers:   blockers,
		}
	}
	return recs
}

func TestFindBlockedCycle(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	tests := []struct {
		name         string
		dependencies map[string][]string
		item         string
		wantAffected []string
		wantPaths    [][]string
	}{
		{
			name: "acyclic",
			dependencies: map[string][]string{
				"510": {"441"},
				"441": {"440"},
			},
			item: "510",
		},
		{
			name: "two issue cycle",
			dependencies: map[string][]string{
				"510": {"441"},
				"441": {"510"},
			},
			item:         "510",
			wantAffected: []string{"510", "441"},
			wantPaths:    [][]string{{"510", "441", "510"}},
		},
		{
			name: "self dependency",
			dependencies: map[string][]string{
				"510": {"510"},
			},
			item:         "510",
			wantAffected: []string{"510"},
			wantPaths:    [][]string{{"510", "510"}},
		},
		{
			name: "longer cycle",
			dependencies: map[string][]string{
				"510": {"441"},
				"441": {"442"},
				"442": {"510"},
			},
			item:         "510",
			wantAffected: []string{"510", "441", "442"},
			wantPaths:    [][]string{{"510", "441", "442", "510"}},
		},
		{
			name: "multiple cycles",
			dependencies: map[string][]string{
				"510": {"441", "442"},
				"441": {"510"},
				"442": {"510"},
			},
			item:         "510",
			wantAffected: []string{"510", "441", "442"},
			wantPaths: [][]string{
				{"510", "441", "510"},
				{"510", "442", "510"},
			},
		},
		{
			name: "pull request claim key",
			dependencies: map[string][]string{
				"pr/955": {"956"},
				"956":    {"955"},
			},
			item:         "pr/955",
			wantAffected: []string{"955", "956"},
			wantPaths:    [][]string{{"955", "956", "955"}},
		},
		{
			name: "unrelated reachable cycle",
			dependencies: map[string][]string{
				"510": {"441"},
				"441": {"442"},
				"442": {"441"},
			},
			item: "510",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recs := blockedCycleTestRecords(repo, tc.dependencies)
			got := findBlockedCycle(recs, blockedRecordKey(repo, tc.item))
			var gotAffected []string
			for _, node := range got.Affected {
				gotAffected = append(gotAffected, node.ItemID)
			}
			if !reflect.DeepEqual(gotAffected, tc.wantAffected) {
				t.Errorf("affected = %v, want %v", gotAffected, tc.wantAffected)
			}
			if !reflect.DeepEqual(got.Paths, tc.wantPaths) {
				t.Errorf("paths = %v, want %v", got.Paths, tc.wantPaths)
			}
		})
	}
}

func TestFindBlockedCycleBoundsDenseGraphPaths(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	dependencies := make(map[string][]string)
	const nodes = 12
	for i := 0; i < nodes; i++ {
		itemID := fmt.Sprintf("%d", 500+i)
		for j := 0; j < nodes; j++ {
			dependencies[itemID] = append(dependencies[itemID], fmt.Sprintf("%d", 500+j))
		}
	}

	result := findBlockedCycle(blockedCycleTestRecords(repo, dependencies), blockedRecordKey(repo, "500"))
	if len(result.Affected) != nodes {
		t.Fatalf("affected nodes = %d, want %d", len(result.Affected), nodes)
	}
	if len(result.Paths) != maxBlockedCyclePaths {
		t.Fatalf("representative paths = %d, want cap %d", len(result.Paths), maxBlockedCyclePaths)
	}
	if !result.MorePaths {
		t.Fatal("MorePaths = false, want dense graph truncation reported")
	}
}

func TestFindBlockedCycleCoversAffectedMembersBeforeTruncating(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	dependencies := map[string][]string{
		"500": {"501"},
		"501": {"500", "502", "503"},
		"502": {"501"},
		"503": {"501"},
	}

	result := findBlockedCycle(blockedCycleTestRecords(repo, dependencies), blockedRecordKey(repo, "500"))
	if result.MorePaths {
		t.Fatal("MorePaths = true, want every cycle edge represented within the path cap")
	}
	represented := make(map[string]bool)
	for _, path := range result.Paths {
		for _, itemID := range path {
			represented[itemID] = true
		}
	}
	for _, node := range result.Affected {
		if !represented[node.ItemID] {
			t.Errorf("affected item %s is absent from representative paths %v", node.ItemID, result.Paths)
		}
	}

	dependencies["501"] = append(dependencies["501"], "504")
	dependencies["504"] = []string{"501"}
	result = findBlockedCycle(blockedCycleTestRecords(repo, dependencies), blockedRecordKey(repo, "500"))
	if len(result.Paths) != maxBlockedCyclePaths {
		t.Fatalf("representative paths = %d, want cap %d", len(result.Paths), maxBlockedCyclePaths)
	}
	if !result.MorePaths {
		t.Fatal("MorePaths = false, want the unrepresented affected member reported")
	}
}

func TestBlockedRecordsLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, blockedRecordsFileName)

	// A missing file is an empty map, not an error — the steady state before
	// any run has ever reported blocked.
	recs, err := loadBlockedRecords(path)
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("recs = %+v, want empty for a missing file", recs)
	}

	recs["510"] = blockedRecord{Blockers: []string{"441", "442"}, RunID: "run-1", Stage: "implement", Reason: "unmet dependency"}
	if err := saveBlockedRecords(path, recs); err != nil {
		t.Fatalf("saveBlockedRecords: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp file left behind after save: %v", err)
	}

	reloaded, err := loadBlockedRecords(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded["510"]; got.RunID != "run-1" || len(got.Blockers) != 2 {
		t.Fatalf("reloaded record = %+v, want RunID run-1 with 2 blockers", got)
	}
}

// TestUpdateBlockedRecordsReblockUpdatesInPlace is QA-1's first gate
// condition: a re-blocked item with a DIFFERENT blocker set updates the
// existing entry rather than accumulating a second one — blocked.json is
// keyed by item id, so a re-record is necessarily an overwrite, never an
// append; this pins that behavior against a regression (e.g. switching the
// map to a slice).
func TestUpdateBlockedRecordsReblockUpdatesInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, blockedRecordsFileName)

	write := func(blockers []string, runID string) {
		recs, err := loadBlockedRecords(path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		recs["510"] = blockedRecord{Blockers: blockers, RunID: runID}
		if err := saveBlockedRecords(path, recs); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	write([]string{"441", "442"}, "run-1")
	write([]string{"445"}, "run-2") // re-blocked on a different, unrelated issue

	recs, err := loadBlockedRecords(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("record count = %d, want exactly 1 (overwrite, not accumulate)", len(recs))
	}
	got := recs["510"]
	if got.RunID != "run-2" || len(got.Blockers) != 1 || got.Blockers[0] != "445" {
		t.Fatalf("record = %+v, want the LATEST block (run-2, blocked on 445)", got)
	}
}

// blockedFilterFixture wires a fake GitHub server + provider for
// filterBlockedEligibility unit tests, independent of the full CLI/instance
// plumbing — these exercise the filter function directly against a
// controlled set of open/closed issues.
func blockedFilterFixture(t *testing.T) (*fakeGitHubServer, *providers.GitHubProvider, providers.RepositoryRef) {
	t.Helper()
	server := newFakeGitHubServer(t, "acme", "web")
	provider := server.newGitHubProvider("test-token")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	return server, provider, repo
}

type stalledIssueClient struct {
	next    providers.HTTPClient
	path    string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *stalledIssueClient) Do(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, c.path) {
		c.once.Do(func() { close(c.started) })
		select {
		case <-c.release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return c.next.Do(req)
}

func TestFilterBlockedEligibilityNoRecordsIsNoop(t *testing.T) {
	_, provider, repo := blockedFilterFixture(t)
	eligible := []providers.WorkItem{{ID: "510"}, {ID: "511"}}
	filtered, skipped, changed, warnings := filterBlockedEligibility(context.Background(), provider, repo, eligible, map[string]blockedRecord{})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", skipped)
	}
	if changed {
		t.Fatal("changed = true, want false for an empty records map")
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered = %v, want both items untouched", filtered)
	}
}

// TestFilterBlockedEligibilitySkipsWhileBlockerOpen is #552's core skip AC: an
// item with a recorded block on a still-open blocker is removed from the
// eligible set, and the record survives untouched (not a false self-heal).
func TestFilterBlockedEligibilitySkipsWhileBlockerOpen(t *testing.T) {
	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "prerequisite", "goobers:ready") // stays open
	server.addIssue(510, "blocked item", "goobers:ready")
	server.addIssue(511, "unrelated item", "goobers:ready")

	eligible := []providers.WorkItem{{ID: "510"}, {ID: "511"}}
	recordKey := blockedRecordKey(repo, "510")
	recs := map[string]blockedRecord{
		recordKey: {Repository: repo, ItemID: "510", Blockers: []string{"441"}, RunID: "run-1"},
	}

	filtered, skipped, changed, warnings := filterBlockedEligibility(context.Background(), provider, repo, eligible, recs)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if changed {
		t.Fatal("changed = true, want false — the blocker is still open, nothing to persist")
	}
	if len(filtered) != 1 || filtered[0].ID != "511" {
		t.Fatalf("filtered = %v, want only 511 (510 skipped, its blocker 441 still open)", filtered)
	}
	if len(skipped) != 1 || skipped[0].reason() != "learned block: item 510 parked on open blocker(s): 441" {
		t.Fatalf("skipped = %+v, want a journal-ready reason for item 510 on blocker 441", skipped)
	}
	if _, ok := recs[recordKey]; !ok {
		t.Fatal("record for 510 was removed, want it to survive (blocker still open)")
	}
}

func TestFilterBlockedEligibilityScopesSameNumberByRepository(t *testing.T) {
	server, provider, webRepo := blockedFilterFixture(t)
	server.addIssue(441, "web prerequisite", "goobers:ready")
	server.addIssue(510, "web blocked item", "goobers:ready")
	server.addIssue(511, "web unrelated item", "goobers:ready")
	apiRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	webRecord := blockedRecord{Repository: webRepo, ItemID: "510", Blockers: []string{"441"}, RunID: "web-run"}
	apiRecord := blockedRecord{Repository: apiRepo, ItemID: "510", Blockers: []string{"999"}, RunID: "api-run"}

	recs := map[string]blockedRecord{
		blockedRecordKey(webRepo, "510"): webRecord,
		blockedRecordKey(apiRepo, "510"): apiRecord,
	}
	filtered, _, changed, warnings := filterBlockedEligibility(
		context.Background(), provider, webRepo,
		[]providers.WorkItem{{ID: "510"}, {ID: "511"}}, recs,
	)
	if changed || len(warnings) != 0 {
		t.Fatalf("changed = %v, warnings = %v; want only the web record evaluated cleanly", changed, warnings)
	}
	if len(filtered) != 1 || filtered[0].ID != "511" {
		t.Fatalf("filtered = %v, want web#510 skipped and web#511 retained", filtered)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %+v, want both repositories retained", recs)
	}

	filtered, _, changed, warnings = filterBlockedEligibility(
		context.Background(),
		provider,
		webRepo,
		[]providers.WorkItem{{ID: "510"}, {ID: "511"}},
		map[string]blockedRecord{blockedRecordKey(apiRepo, "510"): apiRecord},
	)
	if changed || len(warnings) != 0 {
		t.Fatalf("other-repo-only changed = %v, warnings = %v; want a no-op", changed, warnings)
	}
	if len(filtered) != 2 {
		t.Fatalf("other-repo-only filtered = %v, want both web items eligible", filtered)
	}
}

func TestFilterBlockedEligibilityUsesMigratedLegacyRecords(t *testing.T) {
	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "prerequisite", "goobers:ready")
	server.addIssue(510, "legacy blocked item", "goobers:ready")
	server.addIssue(511, "scoped blocked item", "goobers:ready")
	server.addIssue(512, "unrelated item", "goobers:ready")
	scopedKey := blockedRecordKey(repo, "511")
	recs := map[string]blockedRecord{
		"510": {Blockers: []string{"441"}, RunID: "legacy-run"},
		scopedKey: {
			Repository: repo,
			ItemID:     "511",
			Blockers:   []string{"441"},
			RunID:      "scoped-run",
		},
	}
	if !migrateLegacyBlockedRecords(recs, repo) {
		t.Fatal("legacy migration reported no change")
	}

	filtered, _, changed, warnings := filterBlockedEligibility(
		context.Background(),
		provider,
		repo,
		[]providers.WorkItem{{ID: "510"}, {ID: "511"}, {ID: "512"}},
		recs,
	)
	if changed || len(warnings) != 0 {
		t.Fatalf("changed = %v, warnings = %v; want both records retained without warnings", changed, warnings)
	}
	if len(filtered) != 1 || filtered[0].ID != "512" {
		t.Fatalf("filtered = %v, want only unrelated item 512", filtered)
	}
	if _, legacy := recs["510"]; legacy {
		t.Fatalf("records = %+v, want the legacy key removed", recs)
	}
	if migrated, ok := recs[blockedRecordKey(repo, "510")]; !ok || migrated.Repository != repo || migrated.ItemID != "510" {
		t.Fatalf("records = %+v, want legacy record scoped to the active repository", recs)
	}
}

func TestFilterBlockedEligibilityMixedBlockerStatesRemainParked(t *testing.T) {
	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "closed prerequisite", "goobers:ready")
	server.addIssue(442, "open prerequisite", "goobers:ready")
	server.addIssue(510, "blocked item", "goobers:ready")
	server.closeIssue(441)

	recordKey := blockedRecordKey(repo, "510")
	recs := map[string]blockedRecord{recordKey: {Repository: repo, ItemID: "510", Blockers: []string{"442", "441"}, RunID: "run-1"}}
	filtered, skipped, changed, warnings := filterBlockedEligibility(
		context.Background(),
		provider,
		repo,
		[]providers.WorkItem{{ID: "510"}},
		recs,
	)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(filtered) != 0 || !changed {
		t.Fatalf("filtered = %v, changed = %v; want item parked with resolved blocker pruned", filtered, changed)
	}
	if len(skipped) != 1 || skipped[0].reason() != "learned block: item 510 parked on open blocker(s): 442" {
		t.Fatalf("skipped = %+v, want only the still-open blocker 442", skipped)
	}
	if got := recs[recordKey].Blockers; !slices.Equal(got, []string{"442"}) {
		t.Fatalf("record blockers = %v, want only still-open blocker 442", got)
	}
}

func TestFilterBlockedEligibilityProviderFailureKeepsAffectedItemParked(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/509"):
			_, _ = w.Write([]byte(`{"number":509,"state":"closed"}`))
		case strings.HasSuffix(r.URL.Path, "/issues/510"):
			_, _ = w.Write([]byte(`{"number":510,"state":"open"}`))
		case strings.HasSuffix(r.URL.Path, "/issues/441"):
			http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	provider := providers.NewGitHubProvider("test-token", func(p *providers.GitHubProvider) {
		p.BaseURL = api.URL
	})
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	key509 := blockedRecordKey(repo, "509")
	key510 := blockedRecordKey(repo, "510")
	recs := map[string]blockedRecord{
		key509: {Repository: repo, ItemID: "509", Blockers: []string{"440"}, RunID: "old-run"},
		key510: {Repository: repo, ItemID: "510", Blockers: []string{"441"}, RunID: "run-1"},
	}

	filtered, skipped, changed, warnings := filterBlockedEligibility(
		context.Background(),
		provider,
		repo,
		[]providers.WorkItem{{ID: "510"}},
		recs,
	)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "check blocker 441 for 510") {
		t.Fatalf("warnings = %v, want blocker lookup failure", warnings)
	}
	if !changed {
		t.Fatal("changed = false, want the independently closed record pruned")
	}
	if len(filtered) != 0 {
		t.Fatalf("filtered = %v, want issue 510 parked while its blocker state is unresolved", filtered)
	}
	if len(skipped) != 1 || skipped[0].reason() != "learned block: item 510 parked; blocker state unresolved: 441" {
		t.Fatalf("skipped = %+v, want issue 510 parked on unresolved blocker 441", skipped)
	}
	if len(recs) != 1 {
		t.Fatalf("records after provider failure = %+v, want only affected record 510 preserved", recs)
	}
	if _, ok := recs[key509]; ok {
		t.Fatal("closed record 509 survived an unrelated blocker lookup failure")
	}
	if _, ok := recs[key510]; !ok {
		t.Fatal("record 510 was removed after its blocker lookup failed")
	}
}

func TestFilterBlockedEligibilityProviderFailureKeepsUnresolvedItemParked(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/issues/510") {
			http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)
	provider := providers.NewGitHubProvider("test-token", func(p *providers.GitHubProvider) {
		p.BaseURL = api.URL
	})
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	key510 := blockedRecordKey(repo, "510")
	recs := map[string]blockedRecord{
		key510: {Repository: repo, ItemID: "510", Blockers: []string{"441"}, RunID: "run-1"},
	}

	filtered, skipped, changed, warnings := filterBlockedEligibility(
		context.Background(),
		provider,
		repo,
		[]providers.WorkItem{{ID: "510"}, {ID: "511"}},
		recs,
	)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "check blocked item 510") {
		t.Fatalf("warnings = %v, want blocked-item lookup failure", warnings)
	}
	if changed {
		t.Fatal("changed = true, want unresolved record preserved unchanged")
	}
	if len(filtered) != 1 || filtered[0].ID != "511" {
		t.Fatalf("filtered = %v, want unresolved issue 510 parked and unrelated issue 511 eligible", filtered)
	}
	if len(skipped) != 1 || skipped[0].reason() != "learned block: item 510 parked; item state unresolved" {
		t.Fatalf("skipped = %+v, want issue 510 parked on unresolved item state", skipped)
	}
	if _, ok := recs[key510]; !ok {
		t.Fatal("record for unresolved issue 510 was removed")
	}
}

// TestFilterBlockedEligibilitySelfHealsWhenBlockersClose is QA-1's required
// self-heal test (gate condition 1): once every one of a record's blockers
// closes, the record clears and the item becomes eligible again — the actual
// #552 acceptance criterion, not just the skip half.
func TestFilterBlockedEligibilitySelfHealsWhenBlockersClose(t *testing.T) {
	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "prerequisite one", "goobers:ready")
	server.addIssue(442, "prerequisite two", "goobers:ready")
	server.addIssue(510, "blocked item", "goobers:ready")
	server.closeIssue(441)
	server.closeIssue(442)

	eligible := []providers.WorkItem{{ID: "510"}}
	recordKey := blockedRecordKey(repo, "510")
	recs := map[string]blockedRecord{
		recordKey: {Repository: repo, ItemID: "510", Blockers: []string{"441", "442"}, RunID: "run-1"},
	}

	filtered, skipped, changed, warnings := filterBlockedEligibility(context.Background(), provider, repo, eligible, recs)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none once all blockers close", skipped)
	}
	if !changed {
		t.Fatal("changed = false, want true — the self-healed record must be persisted")
	}
	if len(filtered) != 1 || filtered[0].ID != "510" {
		t.Fatalf("filtered = %v, want 510 eligible again (both blockers closed)", filtered)
	}
	if _, ok := recs[recordKey]; ok {
		t.Fatal("record for 510 still present, want it cleared by self-heal — no human involved")
	}
}

// TestFilterBlockedEligibilityPrunesRecordForClosedItem is QA-1's second gate
// condition: a record for an issue that itself closed (by any path — manual
// close, curation, a downstream workflow) must not linger as dead weight,
// even though nothing self-healed it.
func TestFilterBlockedEligibilityPrunesRecordForClosedItem(t *testing.T) {
	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "prerequisite", "goobers:ready") // still open
	server.addIssue(510, "blocked item, now closed", "goobers:ready")
	server.closeIssue(510)

	// 510 no longer appears in this tick's eligible set (it's closed, so the
	// provider query wouldn't return it) — filterBlockedEligibility must still
	// prune its stale record via the direct GetWorkItem check.
	filtered, skipped, changed, warnings := filterBlockedEligibility(context.Background(), provider, repo, nil, map[string]blockedRecord{
		blockedRecordKey(repo, "510"): {
			Repository: repo,
			ItemID:     "510",
			Blockers:   []string{"441"},
			RunID:      "run-1",
		},
	})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none for a closed item", skipped)
	}
	if !changed {
		t.Fatal("changed = false, want true — the stale record must be persisted as removed")
	}
	if len(filtered) != 0 {
		t.Fatalf("filtered = %v, want empty", filtered)
	}
}

// TestBacklogQueryRechecksBlockedRecordsBeforeClaim recreates #722's race:
// provider eligibility has finished, but a learned block arrives before the
// ledger acquisition. The claim transaction must observe that write.
func TestBacklogQueryRechecksBlockedRecordsBeforeClaim(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(441, "open prerequisite", "goobers:approved")
	server.addIssue(510, "blocked item", "goobers:approved", "goobers:ready")

	l := layoutFor(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-race")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	workDir := t.TempDir()
	t.Chdir(workDir)

	claimReady := make(chan struct{})
	resumeClaim := make(chan struct{})
	defer func() {
		select {
		case <-resumeClaim:
		default:
			close(resumeClaim)
		}
	}()

	type queryResult struct {
		code   int
		stdout string
		stderr string
	}
	queryDone := make(chan queryResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := runBacklogQueryWithClaimBarrier([]string{"--claim", root}, &stdout, &stderr, func() {
			close(claimReady)
			<-resumeClaim
		})
		queryDone <- queryResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()

	select {
	case <-claimReady:
	case <-time.After(5 * time.Second):
		t.Fatal("backlog-query did not reach the claim transaction")
	}

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	concurrentKey := blockedRecordKey(repo, "510")
	if err := updateBlockedRecords(l, func(recs map[string]blockedRecord) bool {
		recs[concurrentKey] = blockedRecord{Repository: repo, ItemID: "510", Blockers: []string{"441"}, RunID: "prior-run"}
		return true
	}); err != nil {
		t.Fatalf("write concurrent blocked record: %v", err)
	}
	close(resumeClaim)

	var result queryResult
	select {
	case result = <-queryDone:
	case <-time.After(5 * time.Second):
		t.Fatal("backlog-query did not finish after blocked record was written")
	}
	if result.code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", result.code, result.stdout, result.stderr)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if entry, ok := ledger.Lookup("510"); ok {
		t.Fatalf("issue 510 was claimed after its blocked record arrived: %+v", entry)
	}
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatalf("read instance journal: %v", err)
	}
	skipFound := false
	for _, event := range events {
		if event.Type == journal.EventClaimAcquired && event.Name == "510" {
			t.Fatal("journal contains a claim for issue 510 after its blocked record arrived")
		}
		if event.Type == journal.EventRunnerAnnotation &&
			event.Runner["annotation"] == blockedEligibilitySkipAnnotation &&
			event.Runner["itemId"] == "510" {
			skipFound = true
		}
	}
	if !skipFound {
		t.Fatal("journal does not contain the blocked eligibility skip for issue 510")
	}
	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("load blocked records: %v", err)
	}
	if _, ok := recs[concurrentKey]; !ok {
		t.Fatal("concurrent blocked record for issue 510 was not preserved")
	}
}

// TestBacklogQuerySkipsKnownBlockedThenSelfHeals covers #792's repeated-claim
// shape: a blocked record remains parked without new claims until its final
// blocker closes, then becomes eligible in that same query cycle.
func TestBacklogQuerySkipsKnownBlockedThenSelfHeals(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(441, "closed prerequisite", "goobers:approved")
	server.addIssue(442, "open prerequisite", "goobers:approved")
	server.addIssue(510, "blocked item", "goobers:approved", "goobers:ready")
	server.closeIssue(441)

	l := layoutFor(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	recs := map[string]blockedRecord{"510": {Blockers: []string{"442", "441"}, RunID: "prior-run"}}
	if err := saveBlockedRecords(blockedRecordsPath(l), recs); err != nil {
		t.Fatalf("seed blocked.json: %v", err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-2")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	workDir := t.TempDir()
	t.Chdir(workDir)

	// First tick: 442 is still open, so 510 is skipped — nothing else eligible,
	// clean no-work exit (#233's contract), not a business error.
	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("first backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var noWork map[string]interface{}
	if err := json.Unmarshal(data, &noWork); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if noWork["claimed"] != false {
		t.Fatalf("first tick claimed = %v, want false (510's blocker 442 is still open)", noWork["claimed"])
	}
	reloaded, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("reload migrated blocked.json: %v", err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	if _, legacy := reloaded["510"]; legacy {
		t.Fatalf("blocked.json after first tick = %+v, want legacy key migrated", reloaded)
	}
	migrated, ok := reloaded[blockedRecordKey(repo, "510")]
	if !ok || migrated.Repository != repo || migrated.ItemID != "510" {
		t.Fatalf("blocked.json after first tick = %+v, want scoped migrated record", reloaded)
	}
	if !slices.Equal(migrated.Blockers, []string{"442"}) {
		t.Fatalf("blocked.json after first tick = %+v, want only open blocker 442 (441 self-healed)", reloaded)
	}

	// Repeating the query without a blocker-state change must remain no-work,
	// rather than re-claiming and re-running the same blocked issue.
	t.Setenv("GOOBERS_RUN_ID", "run-3")
	code, stdout, stderr = runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("repeated backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if recs, _ := loadBlockedRecords(blockedRecordsPath(l)); len(recs) != 1 {
		t.Fatalf("blocked.json after repeated tick = %+v, want the record to remain parked", recs)
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatalf("read instance journal: %v", err)
	}
	var skips, claims int
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["annotation"] == blockedEligibilitySkipAnnotation {
			skips++
			if event.Reason != "learned block: item 510 parked on open blocker(s): 442" {
				t.Fatalf("skip reason = %q, want deterministic open-blocker reason", event.Reason)
			}
		}
		if event.Type == journal.EventClaimAcquired && event.Name == "510" {
			claims++
		}
	}
	if skips != 2 || claims != 0 {
		t.Fatalf("journal before final blocker closes: skips=%d claims=%d, want 2 skips and 0 claims", skips, claims)
	}

	// Final blocker closes: self-heal fires and 510 becomes eligible in this
	// query cycle, with exactly one claim transition.
	server.closeIssue(442)
	t.Setenv("GOOBERS_RUN_ID", "run-4")
	code, stdout, stderr = runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("self-healing backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err = os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var claimed map[string]interface{}
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if claimed["id"] != "510" {
		t.Fatalf("self-healing tick claimed id = %v, want 510", claimed["id"])
	}
	if recs, _ := loadBlockedRecords(blockedRecordsPath(l)); len(recs) != 0 {
		t.Fatalf("blocked.json after self-heal = %+v, want empty", recs)
	}

	t.Setenv("GOOBERS_RUN_ID", "run-5")
	if code, _, stderr = runArgs(t, "backlog-query", "--claim", root); code != 0 {
		t.Fatalf("post-heal repeated query: code = %d, stderr = %q", code, stderr)
	}
	events, err = journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatalf("read final instance journal: %v", err)
	}
	claims = 0
	for _, event := range events {
		if event.Type == journal.EventClaimAcquired && event.Name == "510" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("claim transitions after blocker-state change = %d, want exactly 1", claims)
	}
}

// TestFilterBlockedEligibilityResolvesPRPrefixedKey is #971's regression test.
// pr-remediation records its driving item under the claim ledger's name, so
// blocked.json grows "pr/955"-shaped keys alongside issue-driven bare numbers.
// Passed through verbatim, that produced GET .../issues/pr/955 — a 404 that
// hard-failed every query-backlog tick and took down implementation and
// backlog-curation together. The key must resolve to the pull request and
// drive the ordinary skip logic.
func TestFilterBlockedEligibilityResolvesPRPrefixedKey(t *testing.T) {
	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(956, "sibling pr, still open", "goobers:ready") // blocker
	server.addIssue(955, "the blocked pull request", "goobers:ready")
	server.addIssue(511, "unrelated item", "goobers:ready")

	eligible := []providers.WorkItem{{ID: "955"}, {ID: "511"}}
	recordKey := blockedRecordKey(repo, "pr/955")
	recs := map[string]blockedRecord{
		recordKey: {Repository: repo, ItemID: "pr/955", Blockers: []string{"956"}, RunID: "run-1"},
	}

	filtered, skipped, changed, warnings := filterBlockedEligibility(context.Background(), provider, repo, eligible, recs)
	if len(warnings) != 0 {
		t.Fatalf("pr/-prefixed key produced warnings, want a clean lookup: %v", warnings)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none because the claim key is pr/955, not eligible id 955", skipped)
	}
	if changed {
		t.Fatal("changed = true, want false — the blocker is still open, nothing to persist")
	}
	if _, ok := recs[recordKey]; !ok {
		t.Fatal("record for pr/955 was removed, want it to survive (blocker still open)")
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered = %v, want both items — a pr/ key skips by its own key, not the bare number", filtered)
	}
}

// TestFilterBlockedEligibilityPrunesPRPrefixedKeyWhenMerged proves the prefix
// strip also feeds the prune half: a pr/ record whose pull request has closed
// or merged clears itself, exactly as a bare-numeric issue record does.
func TestFilterBlockedEligibilityPrunesPRPrefixedKeyWhenMerged(t *testing.T) {
	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(955, "the pull request", "goobers:ready")
	server.closeIssue(955)

	recordKey := blockedRecordKey(repo, "pr/955")
	recs := map[string]blockedRecord{
		recordKey: {Repository: repo, ItemID: "pr/955", Blockers: []string{"956"}, RunID: "run-1"},
	}
	_, skipped, changed, warnings := filterBlockedEligibility(context.Background(), provider, repo, nil, recs)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none for a closed pull request", skipped)
	}
	if !changed {
		t.Fatal("changed = false, want true — a closed pull request's record must be pruned")
	}
	if _, ok := recs[recordKey]; ok {
		t.Fatal("record for pr/955 survived, want it pruned once its pull request closed")
	}
}

// TestFilterBlockedEligibilityDegradesOnUnresolvableKey is the other half of
// #971: whatever malformed key ends up in blocked.json next must not halt
// backlog selection. An unresolvable record is reported as a warning and left
// untouched — neither pruned (we cannot prove it closed) nor self-healed —
// while every other record is still processed normally.
func TestFilterBlockedEligibilityDegradesOnUnresolvableKey(t *testing.T) {
	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "prerequisite", "goobers:ready")
	server.addIssue(510, "blocked item", "goobers:ready")
	server.addIssue(511, "unrelated item", "goobers:ready")

	eligible := []providers.WorkItem{{ID: "510"}, {ID: "511"}, {ID: "not-a-real-key"}}
	healthyKey := blockedRecordKey(repo, "510")
	malformedKey := blockedRecordKey(repo, "not-a-real-key")
	candidates := append([]providers.WorkItem(nil), eligible...)
	recs := map[string]blockedRecord{
		healthyKey: {
			Repository: repo,
			ItemID:     "510",
			Blockers:   []string{"441"},
			RunID:      "run-1",
		},
		malformedKey: {
			Repository: repo,
			ItemID:     "not-a-real-key",
			Blockers:   []string{"441"},
			RunID:      "run-2",
		},
	}

	filtered, skipped, _, warnings := filterBlockedEligibility(context.Background(), provider, repo, eligible, recs)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one for the unresolvable key", warnings)
	}
	if !strings.Contains(warnings[0], "not-a-real-key") {
		t.Fatalf("warning = %q, want it to name the offending key", warnings[0])
	}
	if _, ok := recs[malformedKey]; !ok {
		t.Fatal("unresolvable record was pruned, want it left untouched for a human to resolve")
	}
	// The healthy record must still have been applied — one bad key degrades
	// only itself, which is the entire point of the change.
	if len(filtered) != 1 || filtered[0].ID != "511" {
		t.Fatalf("filtered = %v, want only 511 — unresolved record must stay parked without blocking healthy items", filtered)
	}

	path := filepath.Join(t.TempDir(), blockedRecordsFileName)
	if err := saveBlockedRecords(path, recs); err != nil {
		t.Fatal(err)
	}
	verifiedSkips := make(map[string]blockedEligibilitySkip, len(skipped))
	for _, skip := range skipped {
		verifiedSkips[skip.ItemID] = skip
	}
	reconciled, reconciledSkips, err := reconcileBlockedEligibilityLocked(path, repo, append([]providers.WorkItem(nil), candidates...), nil, nil, verifiedSkips)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 1 || reconciled[0].ID != "511" || len(reconciledSkips) != 2 {
		t.Fatalf("reconciled = %v, skips = %+v; want only 511 with both blocked records parked", reconciled, reconciledSkips)
	}

	replacement := recs[malformedKey]
	replacement.RunID = "replacement-run"
	if err := saveBlockedRecords(path, map[string]blockedRecord{malformedKey: replacement}); err != nil {
		t.Fatal(err)
	}
	reconciled, reconciledSkips, err = reconcileBlockedEligibilityLocked(path, repo, append([]providers.WorkItem(nil), candidates...), nil, nil, verifiedSkips)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 2 || len(reconciledSkips) != 1 || !reconciledSkips[0].VerificationPending {
		t.Fatalf("reconciled after replacement = %v, skips = %+v; want the replacement parked pending verification", reconciled, reconciledSkips)
	}
}

func TestBacklogQueryDoesNotClaimConcurrentBlockedRecordReplacement(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(441, "resolved prerequisite", "goobers:approved")
	server.closeIssue(441)
	server.addIssue(442, "new prerequisite", "goobers:approved")
	server.addIssue(510, "blocked item", "goobers:approved", "goobers:ready")

	l := layoutFor(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}

	observed := blockedRecord{
		Repository: repo,
		ItemID:     "510",
		Blockers:   []string{"441"},
		RunID:      "old-run",
		RecordedAt: time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC),
	}
	recordKey := blockedRecordKey(repo, "510")
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{recordKey: observed}); err != nil {
		t.Fatal(err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("510", "old-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed old claim: ok=%v err=%v", ok, err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "query-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	workDir := t.TempDir()
	t.Chdir(workDir)

	stalled := &stalledIssueClient{
		path:    "/issues/441",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	releaseProvider := sync.OnceFunc(func() { close(stalled.release) })
	defer releaseProvider()
	baseFactory := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		provider := baseFactory(token, opts...)
		stalled.next = provider.Client
		provider.Client = stalled
		return provider
	}
	t.Cleanup(func() { newGitHubProvider = baseFactory })

	var stdout, stderr bytes.Buffer
	queryDone := make(chan int, 1)
	go func() {
		queryDone <- runBacklogQuery([]string{"--claim", root}, &stdout, &stderr)
	}()
	select {
	case <-stalled.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for old blocker resolution")
	}

	replacement := blockedRecord{
		Repository: repo,
		ItemID:     "510",
		Blockers:   []string{"442"},
		RunID:      "new-run",
		RecordedAt: time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC),
	}
	if err := updateBlockedRecords(l, func(recs map[string]blockedRecord) bool {
		recs[recordKey] = replacement
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := releaseClaimsForRun(l, nil, "old-run"); err != nil {
		t.Fatal(err)
	}

	releaseProvider()
	if code := <-queryDone; code != 0 {
		t.Fatalf("backlog query code = %d, stderr = %q", code, stderr.String())
	}

	recs, err := snapshotBlockedRecords(l)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := recs[recordKey]; !ok || !sameBlockedRecord(got, replacement) {
		t.Fatalf("concurrent replacement = (%+v, %v), want preserved %+v", got, ok, replacement)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := reopened.Lookup("510"); held {
		t.Fatalf("concurrently re-blocked item was claimed: %+v", entry)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["claimed"] != false {
		t.Fatalf("claimed = %v, want false for concurrently re-blocked item", result["claimed"])
	}
}

func TestStalledBlockedStateProviderCallDoesNotDelayFinalizer(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(441, "prerequisite", "goobers:approved")
	server.addIssue(510, "blocked item", "goobers:approved", "goobers:ready")

	l := layoutFor(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(repo, "510"): {
			Repository: repo,
			ItemID:     "510",
			Blockers:   []string{"441"},
			RunID:      "prior-run",
		},
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("900", "terminal-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed terminal claim: ok=%v err=%v", ok, err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "query-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Chdir(t.TempDir())

	stalled := &stalledIssueClient{
		path:    "/issues/510",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	releaseProvider := sync.OnceFunc(func() { close(stalled.release) })
	defer releaseProvider()
	baseFactory := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		provider := baseFactory(token, opts...)
		stalled.next = provider.Client
		provider.Client = stalled
		return provider
	}
	t.Cleanup(func() { newGitHubProvider = baseFactory })

	var stdout, stderr bytes.Buffer
	queryDone := make(chan int, 1)
	go func() {
		queryDone <- runBacklogQuery([]string{"--claim", root}, &stdout, &stderr)
	}()

	select {
	case <-stalled.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked-state provider call")
	}

	finalizerDone := make(chan error, 1)
	go func() {
		finalizerDone <- releaseClaimsForRun(l, nil, "terminal-run")
	}()
	select {
	case err := <-finalizerDone:
		if err != nil {
			t.Fatalf("finalize terminal claim: %v", err)
		}
	case <-time.After(2 * time.Second):
		releaseProvider()
		<-queryDone
		t.Fatal("terminal finalizer waited for a stalled provider call to release the claims lock")
	}

	releaseProvider()
	if code := <-queryDone; code != 0 {
		t.Fatalf("backlog query code = %d, stderr = %q", code, stderr.String())
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := reopened.Lookup("900"); held {
		t.Fatalf("terminal claim still held after finalizer: %+v", entry)
	}
}
