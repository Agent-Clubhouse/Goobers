package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// Goobers#4036: `query-backlog`'s forward scan reported the backlog drained
// while never looking at the items it would have accepted.
//
// The live signature was exact and is reproduced below: 218 open
// goobers:approved issues, a 250-candidate scan budget that cannot bind, and
// a run that examined 60 of them — every one of which carried an exclude
// label — before writing an empty cursor, i.e. "the provider says the result
// set is exhausted". The 10 items that carried no exclude label were all in
// the 158 it never examined.
//
// The mechanism was the durable scan cursor resting mid-set. A scan that
// resumed there read only the short tail after it and then, because the
// provider truthfully reported no next page for that tail, returned with its
// budget almost entirely unspent, an empty eligible set, and no signal at all
// that it had covered a fraction of the backlog.

// fakeBacklogPageServer models api.github.com's issues collection closely
// enough for cursor arithmetic: a stable oldest-first result set, honest
// per_page/page slicing, and a record of every page request made.
type fakeBacklogPageServer struct {
	*httptest.Server
	requests []string
}

func newFakeBacklogPageServer(t *testing.T, total int) *fakeBacklogPageServer {
	t.Helper()
	server := &fakeBacklogPageServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		perPage, _ := strconv.Atoi(query.Get("per_page"))
		if perPage <= 0 {
			perPage = 30
		}
		page, _ := strconv.Atoi(query.Get("page"))
		if page <= 0 {
			page = 1
		}
		server.requests = append(server.requests, fmt.Sprintf("page=%d per_page=%d", page, perPage))
		start := min((page-1)*perPage, total)
		end := min(start+perPage, total)
		issues := make([]map[string]any, 0, end-start)
		for i := start; i < end; i++ {
			issues = append(issues, map[string]any{
				"id": i + 1, "number": i + 1, "title": "candidate", "state": "open",
				"labels": []map[string]string{{"name": "goobers:approved"}},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issues)
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *fakeBacklogPageServer) provider() *providers.GitHubProvider {
	return providers.NewGitHubProvider("tok", func(p *providers.GitHubProvider) { p.BaseURL = s.URL })
}

func scanWindowIDs(items []providers.WorkItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestBacklogScanWindowWrapsInsteadOfReportingTheBacklogDrained is #4036's
// core acceptance, in the exact live shape: 218 candidates, a cursor resting
// at 158, and a budget of 250 that comfortably covers the whole set. Before
// the fix this returned the 60-item tail and an empty cursor — the provider's
// "no next page" for a tail read, indistinguishable from a drained backlog.
func TestBacklogScanWindowWrapsInsteadOfReportingTheBacklogDrained(t *testing.T) {
	const total = 218
	server := newFakeBacklogPageServer(t, total)

	items, window, err := listBacklogScanWindow(
		context.Background(), server.provider(),
		providers.RepositoryRef{Owner: "acme", Name: "app"},
		[]string{"goobers:approved"}, "", nil,
		backlogScanCeiling, backlogScanCursor{Cursor: "158"}, false,
	)
	if err != nil {
		t.Fatalf("listBacklogScanWindow: %v", err)
	}
	if len(items) != total {
		t.Fatalf("examined %d of %d candidates (ids %v); a %d-candidate budget covers the whole set, so the window must too",
			len(items), total, scanWindowIDs(items), backlogScanCeiling)
	}
	// Every candidate exactly once: the wrap segment overlaps the tail the
	// pre-wrap segment already covered, and the caller must never see an item
	// twice (a duplicate would be double-counted by every downstream filter).
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.ID] {
			t.Fatalf("item %s returned twice by one scan window", item.ID)
		}
		seen[item.ID] = true
	}
	for i := 1; i <= total; i++ {
		if !seen[strconv.Itoa(i)] {
			t.Fatalf("candidate %d was never examined", i)
		}
	}
	if window.Truncated {
		t.Fatalf("window.Truncated = true, want false: the whole result set fit inside the %d-candidate budget", backlogScanCeiling)
	}
	if window.Cursor.Cursor != "" {
		t.Fatalf("window.Cursor = %q, want the zero cursor after a full pass", window.Cursor.Cursor)
	}
	// Fairness is still oldest-first from where the last scan stopped: the
	// resumed tail comes first, then the wrap.
	if items[0].ID != "159" {
		t.Fatalf("first examined item = %s, want 159 (the scan resumes at its stored cursor)", items[0].ID)
	}
}

// TestBacklogScanWindowCursorPastEndOfSetStillCoversTheBacklog is the same
// silent-drain failure at its worst: a cursor beyond the end of a shrunken
// result set returned ZERO candidates and an empty cursor, so the stage
// reported "no eligible item to claim" having looked at nothing at all.
func TestBacklogScanWindowCursorPastEndOfSetStillCoversTheBacklog(t *testing.T) {
	const total = 218
	server := newFakeBacklogPageServer(t, total)

	items, window, err := listBacklogScanWindow(
		context.Background(), server.provider(),
		providers.RepositoryRef{Owner: "acme", Name: "app"},
		[]string{"goobers:approved"}, "", nil,
		backlogScanCeiling, backlogScanCursor{Cursor: "400"}, false,
	)
	if err != nil {
		t.Fatalf("listBacklogScanWindow: %v", err)
	}
	if len(items) != total {
		t.Fatalf("examined %d candidates, want %d: a cursor past the end of the set must wrap, not report exhaustion", len(items), total)
	}
	if window.Truncated || window.Cursor.Cursor != "" {
		t.Fatalf("window = %+v, want a complete, non-truncated pass", window)
	}
}

// TestBacklogScanWindowBudgetTruncationAdvancesCursorWithoutWrapping is the
// negative control for the wrap: when the result set is genuinely larger than
// the scan budget, the window must still stop at the budget, report itself
// truncated, and advance the durable cursor so the NEXT tick picks up where
// this one stopped. Wrapping there would re-read the head every tick and
// starve everything past the first window — the exact fairness property
// #532's cursor exists to provide.
func TestBacklogScanWindowBudgetTruncationAdvancesCursorWithoutWrapping(t *testing.T) {
	const total = 600
	server := newFakeBacklogPageServer(t, total)

	items, window, err := listBacklogScanWindow(
		context.Background(), server.provider(),
		providers.RepositoryRef{Owner: "acme", Name: "app"},
		[]string{"goobers:approved"}, "", nil,
		backlogScanCeiling, backlogScanCursor{}, false,
	)
	if err != nil {
		t.Fatalf("listBacklogScanWindow: %v", err)
	}
	if len(items) != backlogScanCeiling {
		t.Fatalf("examined %d candidates, want exactly the %d-candidate budget", len(items), backlogScanCeiling)
	}
	if !window.Truncated {
		t.Fatalf("window.Truncated = false, want true: %d candidates remain unexamined", total-len(items))
	}
	if window.Cursor.Cursor == "" {
		t.Fatalf("window.Cursor is the zero cursor, want an advanced cursor so the next tick resumes past %s", items[len(items)-1].ID)
	}
	if items[0].ID != "1" {
		t.Fatalf("first examined item = %s, want 1 (oldest first)", items[0].ID)
	}
}

// TestBacklogScanWindowDoesNotWrapFromTheZeroCursor keeps the wrap from
// costing a request it cannot need: a scan that already started at the
// beginning and ran out of result set has seen everything by construction.
func TestBacklogScanWindowDoesNotWrapFromTheZeroCursor(t *testing.T) {
	server := newFakeBacklogPageServer(t, 40)

	items, window, err := listBacklogScanWindow(
		context.Background(), server.provider(),
		providers.RepositoryRef{Owner: "acme", Name: "app"},
		[]string{"goobers:approved"}, "", nil,
		backlogScanCeiling, backlogScanCursor{}, false,
	)
	if err != nil {
		t.Fatalf("listBacklogScanWindow: %v", err)
	}
	if len(items) != 40 || window.Truncated || window.Cursor.Cursor != "" {
		t.Fatalf("items = %d, window = %+v; want a single complete pass", len(items), window)
	}
	if len(server.requests) != 1 {
		t.Fatalf("provider requests = %v, want exactly one: nothing to wrap to", server.requests)
	}
}

// TestBacklogScanWindowBoundsCandidateSpendAcrossTheWrap holds the bound the
// wrap must not break: the budget is a spend limit across BOTH segments, not
// per segment, so a wrapping scan can never cost more provider reads than a
// non-wrapping one of the same budget.
func TestBacklogScanWindowBoundsCandidateSpendAcrossTheWrap(t *testing.T) {
	server := newFakeBacklogPageServer(t, 5000)

	_, window, err := listBacklogScanWindow(
		context.Background(), server.provider(),
		providers.RepositoryRef{Owner: "acme", Name: "app"},
		[]string{"goobers:approved"}, "", nil,
		backlogScanCeiling, backlogScanCursor{Cursor: "4950"}, false,
	)
	if err != nil {
		t.Fatalf("listBacklogScanWindow: %v", err)
	}
	// The budget is checked after each page, so the last page may overshoot
	// by at most one page width — the pre-existing contract. What must hold
	// is that the wrap adds no second budget.
	if window.Spent > backlogScanCeiling+backlogScanPageSize {
		t.Fatalf("spent %d candidates, want at most the %d-candidate budget plus one %d-record page",
			window.Spent, backlogScanCeiling, backlogScanPageSize)
	}
	if len(server.requests) > (backlogScanCeiling/backlogScanPageSize)+2 {
		t.Fatalf("provider requests = %v, want the wrap to share the scan's page budget", server.requests)
	}
}

// TestBacklogQueryResumedScanClaimsTheEligibleItemItUsedToSkip is #4036 at
// the stage level, wired exactly as `query-backlog` was live: trustLabel
// goobers:approved, the five exclude labels, maxItems 20, 218 approved open
// issues of which only ten carry no exclude label — and a durable scan cursor
// resting mid-set from a previous tick. Before the fix this run examined the
// 60-candidate tail, rejected every one of them, and reported "no work: no
// eligible item to claim" with an empty cursor, which is precisely how a
// backlog holding 158 unexamined approved items read as converged for days.
func TestBacklogQueryResumedScanClaimsTheEligibleItemItUsedToSkip(t *testing.T) {
	const (
		total        = 218
		resumeCursor = "158"
	)
	excludeLabels := []string{
		providers.LabelReady,
		providers.LabelNeedsHuman,
		blockedOnSiblingLabel,
		"goobers:needs-remediation",
		"goobers:local",
	}
	// The ten label-eligible items sit where the live ones did: inside the
	// range the broken scan "covered", but never in the window it read.
	eligibleIDs := map[int]bool{12: true, 31: true, 57: true, 74: true, 96: true, 110: true, 123: true, 140: true, 151: true, 157: true}

	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for i := 1; i <= total; i++ {
		if eligibleIDs[i] {
			server.addIssue(i, fmt.Sprintf("Item %d", i), "goobers:approved")
			continue
		}
		server.addIssue(i, fmt.Sprintf("Item %d", i), "goobers:approved", excludeLabels[i%len(excludeLabels)])
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-4036")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", strings.Join(excludeLabels, ","))
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")

	repo, err := providerRepo(root)
	if err != nil {
		t.Fatalf("provider repo: %v", err)
	}
	cursorKey := backlogScanCursorKey(
		backlogRepoRefForStage(root, repo), "goobers:approved", "", "", nil, excludeLabels, "",
	)
	cursorPath := filepath.Join(root, "scheduler", cursorKey)
	if err := os.WriteFile(cursorPath, []byte(`{"cursor":"`+resumeCursor+`"}`), 0o644); err != nil {
		t.Fatalf("seed scan cursor: %v", err)
	}

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "backlog-query", "--debug", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	// Proves the seeded cursor is the one this scan actually resumed from,
	// so the assertions below are about the resumed path and not a scan that
	// happened to start at the beginning.
	if !strings.Contains(stderr, `from cursor "`+resumeCursor+`"`) {
		t.Fatalf("stderr = %q, want the scan to report resuming from the stored cursor %s", stderr, resumeCursor)
	}
	if !strings.Contains(stderr, fmt.Sprintf("scan examined %d candidate(s)", total)) {
		t.Fatalf("stderr = %q, want all %d candidates examined", stderr, total)
	}
	if got := strings.Count(stderr, "reached eligibility evaluation"); got != total {
		t.Fatalf("%d candidates reached eligibility evaluation, want all %d — this is the live measurement #4036 was filed on", got, total)
	}
	if strings.Contains(stderr, "eligible set empty") {
		t.Fatalf("stderr = %q, want the ten label-eligible items found, not an empty eligible set", stderr)
	}
	if !strings.Contains(stdout, "claimed 10 items") {
		t.Fatalf("stdout = %q, want all ten label-eligible items claimed", stdout)
	}
	data, err := os.ReadFile("claimed-item.json")
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var claimed []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatalf("unmarshal claimed-item.json (%s): %v", data, err)
	}
	claimedIDs := map[string]bool{}
	for _, item := range claimed {
		claimedIDs[item.ID] = true
	}
	for id := range eligibleIDs {
		if !claimedIDs[strconv.Itoa(id)] {
			t.Fatalf("item %d was label-eligible but never claimed; claimed = %v", id, claimedIDs)
		}
	}
}

// TestBacklogQueryWarnsWhenAnEmptyScanRanOutOfBudget adds the invariant
// #4036 asked for directly: a cycle that finds nothing must say whether it
// looked at the whole backlog or only part of it. With the wrap in place a
// truncated window means exactly one thing — the candidate budget ran out —
// and that, paired with an empty eligible set, is the state an operator needs
// told about rather than reading "no work" as "drained".
func TestBacklogQueryWarnsWhenAnEmptyScanRanOutOfBudget(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for i := 1; i <= backlogScanCeiling+50; i++ {
		server.addIssue(i, fmt.Sprintf("Item %d", i), "goobers:approved", providers.LabelNeedsHuman)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-4036-warn")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", providers.LabelNeedsHuman)

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work: no eligible item to claim") {
		t.Fatalf("stdout = %q, want the routine no-work outcome", stdout)
	}
	if !strings.Contains(stderr, "warning: backlog scan found no eligible item after examining") ||
		!strings.Contains(stderr, "unexamined items remain") {
		t.Fatalf("stderr = %q, want a warning that the empty result came from a truncated scan", stderr)
	}
}

func TestReadOnlyBacklogQueryWarnsWhenAnEmptyScanRanOutOfBudget(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for i := 1; i <= backlogScanCeiling+50; i++ {
		server.addIssue(i, fmt.Sprintf("Item %d", i), "goobers:approved", providers.LabelNeedsHuman)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_READ", "run-4036-readonly-warn")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", providers.LabelNeedsHuman)

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "backlog-query", "--read-only", root)
	if code != 0 {
		t.Fatalf("backlog-query --read-only: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no eligible items") {
		t.Fatalf("stdout = %q, want the read-only no-eligible output", stdout)
	}
	if !strings.Contains(stderr, "warning: backlog scan found no eligible item after examining") ||
		!strings.Contains(stderr, "unexamined items remain") {
		t.Fatalf("stderr = %q, want a truncation warning on the read-only path", stderr)
	}
}

// TestBacklogQueryDoesNotWarnWhenTheBacklogIsGenuinelyDrained is the negative
// control for that warning: it must stay silent when the scan really did
// cover everything, or it becomes noise on every idle tick and stops meaning
// anything.
func TestBacklogQueryDoesNotWarnWhenTheBacklogIsGenuinelyDrained(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for i := 1; i <= 5; i++ {
		server.addIssue(i, fmt.Sprintf("Item %d", i), "goobers:approved", providers.LabelNeedsHuman)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-4036-quiet")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", providers.LabelNeedsHuman)

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work: no eligible item to claim") {
		t.Fatalf("stdout = %q, want the routine no-work outcome", stdout)
	}
	if strings.Contains(stderr, "warning: backlog scan found no eligible item") {
		t.Fatalf("stderr = %q, want no truncation warning: the scan covered the whole backlog", stderr)
	}
}
