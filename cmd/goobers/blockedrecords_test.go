package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/providers"
)

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

func TestFilterBlockedEligibilityNoRecordsIsNoop(t *testing.T) {
	_, provider, repo := blockedFilterFixture(t)
	eligible := []providers.WorkItem{{ID: "510"}, {ID: "511"}}
	filtered, changed, err := filterBlockedEligibility(context.Background(), provider, repo, eligible, map[string]blockedRecord{})
	if err != nil {
		t.Fatalf("filterBlockedEligibility: %v", err)
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
	recs := map[string]blockedRecord{"510": {Blockers: []string{"441"}, RunID: "run-1"}}

	filtered, changed, err := filterBlockedEligibility(context.Background(), provider, repo, eligible, recs)
	if err != nil {
		t.Fatalf("filterBlockedEligibility: %v", err)
	}
	if changed {
		t.Fatal("changed = true, want false — the blocker is still open, nothing to persist")
	}
	if len(filtered) != 1 || filtered[0].ID != "511" {
		t.Fatalf("filtered = %v, want only 511 (510 skipped, its blocker 441 still open)", filtered)
	}
	if _, ok := recs["510"]; !ok {
		t.Fatal("record for 510 was removed, want it to survive (blocker still open)")
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
	recs := map[string]blockedRecord{"510": {Blockers: []string{"441", "442"}, RunID: "run-1"}}

	filtered, changed, err := filterBlockedEligibility(context.Background(), provider, repo, eligible, recs)
	if err != nil {
		t.Fatalf("filterBlockedEligibility: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true — the self-healed record must be persisted")
	}
	if len(filtered) != 1 || filtered[0].ID != "510" {
		t.Fatalf("filtered = %v, want 510 eligible again (both blockers closed)", filtered)
	}
	if _, ok := recs["510"]; ok {
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
	filtered, changed, err := filterBlockedEligibility(context.Background(), provider, repo, nil, map[string]blockedRecord{
		"510": {Blockers: []string{"441"}, RunID: "run-1"},
	})
	if err != nil {
		t.Fatalf("filterBlockedEligibility: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true — the stale record must be persisted as removed")
	}
	if len(filtered) != 0 {
		t.Fatalf("filtered = %v, want empty", filtered)
	}
}

// TestBacklogQuerySkipsKnownBlockedThenSelfHeals is the end-to-end CLI
// acceptance for #552: a blocked.json record for issue 510 blocks it from
// `backlog-query --claim` while its recorded blocker (441) is open, and
// claiming 510 succeeds automatically — no human, no re-record — the tick
// after 441 closes.
func TestBacklogQuerySkipsKnownBlockedThenSelfHeals(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	// 441 is deliberately NOT goobers:ready — it's the prerequisite, not
	// itself a backlog-query candidate; only its open/closed state matters.
	server.addIssue(441, "prerequisite", "goobers:approved")
	server.addIssue(510, "blocked item", "goobers:approved", "goobers:ready")

	l := layoutFor(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	recs := map[string]blockedRecord{"510": {Blockers: []string{"441"}, RunID: "prior-run"}}
	if err := saveBlockedRecords(blockedRecordsPath(l), recs); err != nil {
		t.Fatalf("seed blocked.json: %v", err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-2")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	workDir := t.TempDir()
	t.Chdir(workDir)

	// First tick: 441 still open, so 510 is skipped — nothing else eligible,
	// clean no-work exit (#233's contract), not a business error.
	code, stdout, _ := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("first backlog-query: code = %d, stdout = %q", code, stdout)
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
		t.Fatalf("first tick claimed = %v, want false (510's blocker 441 is still open)", noWork["claimed"])
	}
	if recs, _ := loadBlockedRecords(blockedRecordsPath(l)); len(recs) != 1 {
		t.Fatalf("blocked.json after first tick = %+v, want the record to survive", recs)
	}

	// Second tick: 441 closes — self-heal fires, 510 becomes eligible and is
	// claimed automatically.
	server.closeIssue(441)
	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("second backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
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
		t.Fatalf("second tick claimed id = %v, want 510 (self-healed, no human involved)", claimed["id"])
	}
	if recs, _ := loadBlockedRecords(blockedRecordsPath(l)); len(recs) != 0 {
		t.Fatalf("blocked.json after self-heal = %+v, want empty", recs)
	}
}
