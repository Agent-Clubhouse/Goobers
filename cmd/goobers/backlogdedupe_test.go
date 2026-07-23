package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

type dedupeRecallFixture struct {
	Items                 []providers.WorkItem `json:"items"`
	ClaimedIDs            []string             `json:"claimedIds"`
	KnownDuplicatePairs   []dedupeFixturePair  `json:"knownDuplicatePairs"`
	KnownNonDuplicatePair dedupeFixturePair    `json:"knownNonDuplicatePair"`
	PersonaOnlyBaseline   struct {
		Method          string              `json:"method"`
		IdentifiedPairs []dedupeFixturePair `json:"identifiedPairs"`
	} `json:"personaOnlyBaseline"`
}

type dedupeFixturePair struct {
	OlderID string `json:"olderId"`
	NewerID string `json:"newerId"`
}

func TestDuplicateCandidateRecallImprovesOverPersonaOnly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "backlogdedupe-recall.json"))
	if err != nil {
		t.Fatalf("read recall fixture: %v", err)
	}
	var fixture dedupeRecallFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode recall fixture: %v", err)
	}
	if !strings.Contains(fixture.PersonaOnlyBaseline.Method, "without dedupe candidate pre-pass") {
		t.Fatalf("persona-only baseline method = %q, want recorded no-pre-pass evaluation", fixture.PersonaOnlyBaseline.Method)
	}

	claimed := make(map[string]bool, len(fixture.ClaimedIDs))
	for _, id := range fixture.ClaimedIDs {
		claimed[id] = true
	}
	candidates := surfaceDuplicateCandidates(fixture.Items, claimed)
	got := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		got[dedupePairKey(candidate.Older.ID, candidate.Newer.ID)] = true
	}

	personaOnly := make(map[string]bool, len(fixture.PersonaOnlyBaseline.IdentifiedPairs))
	for _, pair := range fixture.PersonaOnlyBaseline.IdentifiedPairs {
		personaOnly[dedupePairKey(pair.OlderID, pair.NewerID)] = true
	}
	prePassMatches, personaOnlyMatches := 0, 0
	for _, pair := range fixture.KnownDuplicatePairs {
		key := dedupePairKey(pair.OlderID, pair.NewerID)
		if got[key] {
			prePassMatches++
		}
		if personaOnly[key] {
			personaOnlyMatches++
		}
	}
	if personaOnlyMatches == 0 {
		t.Fatal("persona-only fixture baseline must record at least one identified duplicate")
	}
	prePassRecall := float64(prePassMatches) / float64(len(fixture.KnownDuplicatePairs))
	personaOnlyRecall := float64(personaOnlyMatches) / float64(len(fixture.KnownDuplicatePairs))
	if prePassRecall <= personaOnlyRecall {
		t.Fatalf("pre-pass recall = %.2f, want greater than recorded persona-only recall %.2f", prePassRecall, personaOnlyRecall)
	}
	if prePassMatches != len(fixture.KnownDuplicatePairs) {
		t.Fatalf("surfaced %d/%d known duplicate pairs; candidates = %+v", prePassMatches, len(fixture.KnownDuplicatePairs), candidates)
	}
	if got[dedupePairKey(fixture.KnownNonDuplicatePair.OlderID, fixture.KnownNonDuplicatePair.NewerID)] {
		t.Fatalf("hand-picked non-duplicate pair was surfaced: %+v", candidates)
	}
}

func dedupePairKey(olderID, newerID string) string {
	return olderID + "\x00" + newerID
}

func TestSurfaceDuplicateCandidatesRanksStrongSignalsDeterministically(t *testing.T) {
	items := []providers.WorkItem{
		{ID: "10", Title: "Claimed item", Body: "Fixes #77. External ref OPS-9. See https://example.com/incidents/9."},
		{ID: "11", Title: "Different words", Body: "This also Fixes #77."},
		{ID: "12", Title: "Another request", Body: "External ref ops-9."},
		{ID: "13", Title: "Unrelated title", Body: "See https://example.com/incidents/9#timeline."},
	}

	got := surfaceDuplicateCandidates(items, map[string]bool{"10": true})
	if len(got) != 3 {
		t.Fatalf("candidate count = %d, want 3: %+v", len(got), got)
	}
	wantIDs := []string{"11", "12", "13"}
	wantScores := []int{100, 95, 90}
	for i := range wantIDs {
		if got[i].Newer.ID != wantIDs[i] || got[i].Score != wantScores[i] {
			t.Fatalf("candidate[%d] = id %s score %d, want id %s score %d", i, got[i].Newer.ID, got[i].Score, wantIDs[i], wantScores[i])
		}
	}
}

func TestSurfaceDuplicateCandidatesRetainsTextDuplicateUnderTaxonomyCrowding(t *testing.T) {
	const maxCandidates = 3
	items := []providers.WorkItem{
		{ID: "10", Title: "CURE-6: Retry scheduler dispatch after restart", Body: "Parent CURE-1. Preserve the lease after failure."},
		{ID: "11", Title: "CURE-7: Refresh website color palette", Body: "Parent CURE-1. Update the visual theme."},
		{ID: "12", Title: "CURE-8: Document release ownership", Body: "Parent CURE-1. Record the deployment contacts."},
		{ID: "13", Title: "CURE-9: Cache account profile images", Body: "Parent CURE-1. Reduce repeated media downloads."},
		{ID: "14", Title: "CURE-10: Export billing audit records", Body: "Parent CURE-1. Add the finance archive."},
		{ID: "20", Title: "Retry scheduler dispatch after daemon restart", Body: "Restore the lease before dispatching the active workflow."},
	}

	candidates := surfaceDuplicateCandidates(items, map[string]bool{"10": true})
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}
	want := dedupePairKey("10", "20")
	for _, candidate := range candidates {
		if dedupePairKey(candidate.Older.ID, candidate.Newer.ID) == want {
			if len(candidate.Signals.SharedExternalReferences) != 0 {
				t.Fatalf("internal taxonomy leaked into external references: %v", candidate.Signals.SharedExternalReferences)
			}
			return
		}
	}
	t.Fatalf("text duplicate %q was crowded out of bounded candidates: %+v", want, candidates)
}

func TestSurfaceDuplicateCandidatesOnlyMarksClaimedNewerAsCloseEligible(t *testing.T) {
	items := []providers.WorkItem{
		{ID: "101", Title: "Prevent duplicate scheduler workflow runs"},
		{ID: "102", Title: "Stop duplicate scheduler workflow runs"},
	}
	tests := []struct {
		name          string
		claimed       map[string]bool
		wantEligible  string
		wantOlderFlag bool
		wantNewerFlag bool
	}{
		{
			name:          "claimed older survivor leaves unclaimed duplicate read-only",
			claimed:       map[string]bool{"101": true},
			wantOlderFlag: true,
		},
		{
			name:          "claimed newer duplicate may be closed",
			claimed:       map[string]bool{"102": true},
			wantEligible:  "102",
			wantNewerFlag: true,
		},
		{
			name:          "both claimed still preserves older survivor",
			claimed:       map[string]bool{"101": true, "102": true},
			wantEligible:  "102",
			wantOlderFlag: true,
			wantNewerFlag: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := surfaceDuplicateCandidates(items, tt.claimed)
			if len(got) != 1 {
				t.Fatalf("candidate count = %d, want 1: %+v", len(got), got)
			}
			candidate := got[0]
			if candidate.CloseEligibleID != tt.wantEligible {
				t.Errorf("closeEligibleId = %q, want %q", candidate.CloseEligibleID, tt.wantEligible)
			}
			if candidate.Older.Claimed != tt.wantOlderFlag || candidate.Newer.Claimed != tt.wantNewerFlag {
				t.Errorf("claimed flags = older:%v newer:%v, want older:%v newer:%v", candidate.Older.Claimed, candidate.Newer.Claimed, tt.wantOlderFlag, tt.wantNewerFlag)
			}
		})
	}
}

func TestBacklogDedupeScansOpenBacklogAndBoundsArtifact(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(101, "Claimed account sync failure", "goobers:approved")
	server.addIssue(900, "Customer import conflict")
	server.addIssue(901, "Another identity incident")
	server.addIssue(902, "Unrelated database work")
	setFakeIssueBody(t, server, 101, "External ref OPS-4421 contains the failing request.")
	setFakeIssueBody(t, server, 900, "Investigate OPS-4421 from the customer report.")
	setFakeIssueBody(t, server, 901, "The same incident is tracked as OPS-4421.")
	setFakeIssueBody(t, server, 902, "Reduce lock contention with randomized retries.")

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	ok, _, err := ledger.Claim("101", "curation-run", "backlog-curation", time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim fixture item: ok=%v err=%v", ok, err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_MAXCANDIDATES", "1")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-dedupe", root)
	if code != 0 {
		t.Fatalf("backlog-dedupe: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "dedupe-candidates.json"))
	if err != nil {
		t.Fatalf("read candidate artifact: %v", err)
	}
	var artifact dedupeCandidateArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode candidate artifact: %v", err)
	}
	if artifact.ScannedItems != 4 {
		t.Fatalf("scannedItems = %d, want all 4 open issues", artifact.ScannedItems)
	}
	if artifact.CandidateCount != 1 || artifact.TotalMatches != 2 || !artifact.Truncated {
		t.Fatalf("artifact bounds = count:%d total:%d truncated:%v, want 1/2/true", artifact.CandidateCount, artifact.TotalMatches, artifact.Truncated)
	}
	if len(artifact.Candidates) != 1 || artifact.Candidates[0].Rank != 1 {
		t.Fatalf("candidates = %+v, want one rank-1 candidate", artifact.Candidates)
	}
	candidate := artifact.Candidates[0]
	if candidate.Older.ID != "101" || candidate.Newer.ID != "900" {
		t.Fatalf("candidate pair = %s/%s, want 101/900", candidate.Older.ID, candidate.Newer.ID)
	}
	if candidate.Newer.Claimed {
		t.Fatal("backlog-wide comparison item should not be marked claimed")
	}
	if candidate.CloseEligibleID != "" {
		t.Fatalf("closeEligibleId = %q, want empty for unclaimed newer comparison item", candidate.CloseEligibleID)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.issues[900].state != "open" || server.issues[901].state != "open" {
		t.Fatal("candidate surfacing must not close issues")
	}
}

func setFakeIssueBody(t *testing.T, server *fakeGitHubServer, number int, body string) {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	issue := server.issues[number]
	if issue == nil {
		t.Fatalf("fixture issue %d does not exist", number)
	}
	issue.body = body
}
