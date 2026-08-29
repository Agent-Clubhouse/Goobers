package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// backlogHealthFixture stands up an instance plus a repository whose issue-event
// history spans several pages, with the ready-label events sitting in the OLDEST
// part of that history. That layout is the point: a resumed scan never re-reads
// those pages, so the ready pool can only be measured from a durable ledger.
func backlogHealthFixture(t *testing.T) (root string, server *fakeGitHubServer) {
	t.Helper()
	root = initDemo(t)
	server = newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Ready", "goobers:approved", "goobers:ready")
	server.addIssue(8, "Parked", "goobers:approved", "goobers:needs-human")
	server.setLabelEventTime(7, providers.LabelReady, true, time.Now().UTC().Add(-2*time.Hour))
	// 250 unrelated events on top: >2 pages of history the walk must cross to
	// reach issue 7's "labeled" event.
	server.addLabelChurn(8, 250)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_READ", "health-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	return root, server
}

func backlogHealthCursorPath(root string) string {
	return instance.NewLayout(root).BacklogHealthCursorPath(
		"", string(providers.ProviderGitHub), "your-org/your-repo", providers.LabelReady)
}

func runBacklogHealthCycle(t *testing.T, root string) backlogHealthReport {
	t.Helper()
	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "backlog-health", root)
	if code != 0 {
		t.Fatalf("backlog-health: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "backlog-health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report backlogHealthReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Scan == nil {
		t.Fatalf("report carries no scan provenance: %#v", report)
	}
	return report
}

// TestBacklogHealthResumesLedgerFromDurableCursor is #3392's regression. Before
// the durable cursor, every cycle re-paginated the repository's entire
// issue-event history — 200+ pages on the live instance, growing with the repo's
// age, spent out of the shared App installation credential until it hit
// remaining 0 roughly every three hours. The second cycle here must read ONE
// page and still resolve a ready item whose label event is three pages back.
func TestBacklogHealthResumesLedgerFromDurableCursor(t *testing.T) {
	root, server := backlogHealthFixture(t)

	first := runBacklogHealthCycle(t, root)
	if first.Scan.Mode != backlogHealthScanFull || first.Scan.Reason != backlogHealthScanFirstRun {
		t.Fatalf("first cycle scan = %#v, want a first-run full scan", *first.Scan)
	}
	if first.Scan.Pages < 3 {
		t.Fatalf("first cycle pages = %d, want the whole history", first.Scan.Pages)
	}
	if first.ReadyPoolDepth != 1 || first.ReadyPoolStarved {
		t.Fatalf("first cycle report = %#v", first)
	}
	if _, err := os.Stat(backlogHealthCursorPath(root)); err != nil {
		t.Fatalf("durable cursor was not persisted: %v", err)
	}
	firstPages := server.issueEventRequestCount()
	if firstPages < 3 {
		t.Fatalf("first cycle issue-event requests = %d, want the full walk", firstPages)
	}

	server.resetIssueEventRequestCount()
	second := runBacklogHealthCycle(t, root)
	if second.Scan.Mode != backlogHealthScanIncremental {
		t.Fatalf("second cycle scan = %#v, want a resumed scan", *second.Scan)
	}
	if got := server.issueEventRequestCount(); got != 1 {
		t.Fatalf("second cycle issue-event requests = %d, want 1 — the whole point of the cursor", got)
	}
	if second.Scan.NewTransitions != 0 {
		t.Fatalf("second cycle new transitions = %d, want none", second.Scan.NewTransitions)
	}
	// The ledger, not the scan, is what still explains the ready pool.
	if second.ReadyPoolDepth != 1 || second.ReadyPoolStarved {
		t.Fatalf("second cycle report = %#v", second)
	}
	if len(second.ReadyTransitions) != len(first.ReadyTransitions) {
		t.Fatalf("ledger shrank across cycles: %d -> %d",
			len(first.ReadyTransitions), len(second.ReadyTransitions))
	}
	if second.AverageReadyAgeSeconds < (2*time.Hour - time.Minute).Seconds() {
		t.Fatalf("resumed ready age = %f, want the original label event's age", second.AverageReadyAgeSeconds)
	}
}

// TestBacklogHealthResumedScanPicksUpNewTransitions proves the cheap path is
// still correct: a label event added after the cursor must land in the ledger.
func TestBacklogHealthResumedScanPicksUpNewTransitions(t *testing.T) {
	root, server := backlogHealthFixture(t)
	first := runBacklogHealthCycle(t, root)

	server.addIssue(9, "Newly ready", "goobers:approved", "goobers:ready")
	second := runBacklogHealthCycle(t, root)
	if second.Scan.Mode != backlogHealthScanIncremental {
		t.Fatalf("scan = %#v, want a resumed scan", *second.Scan)
	}
	if second.Scan.NewTransitions != 1 {
		t.Fatalf("new transitions = %d, want the one new ready event", second.Scan.NewTransitions)
	}
	if second.ReadyPoolDepth != 2 {
		t.Fatalf("ready pool depth = %d, want both ready items", second.ReadyPoolDepth)
	}
	if len(second.ReadyTransitions) != len(first.ReadyTransitions)+1 {
		t.Fatalf("ledger = %d transitions, want the resumed one appended to %d",
			len(second.ReadyTransitions), len(first.ReadyTransitions))
	}
}

// TestBacklogHealthFullRescanOnCorruptCursor keeps a damaged ledger from
// wedging a periodic check: an unreadable cursor falls back to the bounded full
// scan and says so, rather than failing the stage.
func TestBacklogHealthFullRescanOnCorruptCursor(t *testing.T) {
	root, _ := backlogHealthFixture(t)
	runBacklogHealthCycle(t, root)

	if err := os.WriteFile(backlogHealthCursorPath(root), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := runBacklogHealthCycle(t, root)
	if second.Scan.Mode != backlogHealthScanFull ||
		second.Scan.Reason != backlogHealthScanIntegrityMismatch {
		t.Fatalf("scan = %#v, want a full scan on integrity mismatch", *second.Scan)
	}
	if second.ReadyPoolDepth != 1 || second.ReadyPoolStarved {
		t.Fatalf("report = %#v", second)
	}
}

// TestBacklogHealthFullRescanWhenLedgerCannotExplainReadyPool covers the other
// integrity mismatch: the cursor parses, but its ledger no longer accounts for
// an item that currently carries the ready label. The stage must rescan rather
// than report a ready pool it cannot substantiate.
func TestBacklogHealthFullRescanWhenLedgerCannotExplainReadyPool(t *testing.T) {
	root, _ := backlogHealthFixture(t)
	runBacklogHealthCycle(t, root)

	path := backlogHealthCursorPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cursor backlogHealthCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		t.Fatal(err)
	}
	cursor.Transitions = nil
	hollow, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, hollow, 0o644); err != nil {
		t.Fatal(err)
	}

	second := runBacklogHealthCycle(t, root)
	if second.Scan.Mode != backlogHealthScanFull ||
		second.Scan.Reason != backlogHealthScanLedgerMismatch {
		t.Fatalf("scan = %#v, want a forced full rescan", *second.Scan)
	}
	if second.ReadyPoolDepth != 1 || len(second.ReadyTransitions) == 0 {
		t.Fatalf("report = %#v, want the rebuilt ledger", second)
	}
}

// TestBacklogHealthDefersBelowRateLimitFloor is #3392's second half: a periodic,
// self-healing check must stand down while the shared credential still has
// budget instead of paging on to remaining 0 and starving claims, label writes,
// and merge-review for the rest of the window.
func TestBacklogHealthDefersBelowRateLimitFloor(t *testing.T) {
	root, server := backlogHealthFixture(t)
	// 8 of 100 remaining is below the 10% floor.
	server.setIssueEventQuota(100, 8, 0)

	report := runBacklogHealthCycle(t, root)
	if !report.Scan.Deferred || report.Scan.DeferReason != providers.LabelTransitionScanStopQuotaFloor {
		t.Fatalf("scan = %#v, want a quota-floor deferral", *report.Scan)
	}
	if server.issueEventRequestCount() > 2 {
		t.Fatalf("issue-event requests = %d, want the walk to stand down immediately",
			server.issueEventRequestCount())
	}
	// A deferred cycle measured nothing, so it must not look like a starved
	// ready pool to the telemetry rollup — which keys on readyPoolObservedAt.
	if report.ReadyPoolObservedAt != "" || report.ReadyPoolStarved {
		t.Fatalf("deferred report = %#v, want no ready-pool observation", report)
	}
	if _, err := os.Stat(backlogHealthCursorPath(root)); !os.IsNotExist(err) {
		t.Fatalf("a truncated walk advanced the cursor (stat err = %v)", err)
	}

	// With the window restored the very next cycle completes normally.
	server.setIssueEventQuota(0, 0, 0)
	recovered := runBacklogHealthCycle(t, root)
	if recovered.Scan.Deferred || recovered.ReadyPoolDepth != 1 {
		t.Fatalf("recovered report = %#v", recovered)
	}
}

// TestBacklogHealthRejectsInvalidScanInputs keeps the new bounds honest: a
// nonsense bound is a usage error, not a silently-ignored input.
func TestBacklogHealthRejectsInvalidScanInputs(t *testing.T) {
	for _, tc := range []struct{ env, value string }{
		{"GOOBERS_INPUT_TRANSITIONSCANMAXPAGES", "0"},
		{"GOOBERS_INPUT_TRANSITIONSCANMAXPAGES", "many"},
		{"GOOBERS_INPUT_TRANSITIONSCANQUOTAFLOOR", "1"},
		{"GOOBERS_INPUT_TRANSITIONSCANQUOTAFLOOR", "-0.5"},
	} {
		t.Run(tc.env+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			code, _, stderr := runArgs(t, "backlog-health", t.TempDir())
			if code != 2 {
				t.Fatalf("code = %d, stderr = %q, want a usage error", code, stderr)
			}
		})
	}
}

func TestMergeLabelTransitionsDeduplicatesOnEventID(t *testing.T) {
	at := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	ledger := []providers.WorkItemLabelTransition{
		{EventID: 2, ItemID: "7", Label: providers.LabelReady, Added: true, OccurredAt: at},
		{EventID: 4, ItemID: "8", Label: providers.LabelReady, Added: true, OccurredAt: at.Add(time.Hour)},
	}
	// A resumed scan always re-reads the page its cursor sits on, so overlap is
	// the normal case rather than an anomaly.
	fresh := []providers.WorkItemLabelTransition{
		{EventID: 4, ItemID: "8", Label: providers.LabelReady, Added: true, OccurredAt: at.Add(time.Hour)},
		{EventID: 6, ItemID: "7", Label: providers.LabelReady, Added: false, OccurredAt: at.Add(2 * time.Hour)},
	}
	merged := mergeLabelTransitions(ledger, fresh)
	if len(merged) != 3 {
		t.Fatalf("merged = %#v, want three distinct events", merged)
	}
	for i, want := range []int64{2, 4, 6} {
		if merged[i].EventID != want {
			t.Fatalf("merged[%d].EventID = %d, want %d", i, merged[i].EventID, want)
		}
	}
}

func TestReadBacklogHealthCursorRejectsMisKeyedLedger(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")

	if _, reason := readBacklogHealthCursor(path, "", repo, providers.LabelReady); reason != backlogHealthScanFirstRun {
		t.Fatalf("missing cursor reason = %q, want %q", reason, backlogHealthScanFirstRun)
	}

	valid := backlogHealthCursor{
		Schema:           backlogHealthCursorSchema,
		Provider:         string(providers.ProviderGitHub),
		Repository:       "your-org/your-repo",
		Label:            providers.LabelReady,
		HighWaterEventID: 10,
		Transitions: []providers.WorkItemLabelTransition{
			{EventID: 4, ItemID: "7", Label: providers.LabelReady, Added: true, OccurredAt: time.Now().UTC()},
		},
	}
	if err := writeBacklogHealthCursor(path, valid); err != nil {
		t.Fatal(err)
	}
	if _, reason := readBacklogHealthCursor(path, "", repo, providers.LabelReady); reason != "" {
		t.Fatalf("valid cursor reason = %q, want it accepted", reason)
	}
	// A cursor keyed to another repository, gaggle, or label must never be
	// resumed: its high-water mark describes a different event stream.
	other := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "other-repo"}
	if _, reason := readBacklogHealthCursor(path, "", other, providers.LabelReady); reason != backlogHealthScanIntegrityMismatch {
		t.Fatalf("mis-keyed repo reason = %q", reason)
	}
	if _, reason := readBacklogHealthCursor(path, "prod", repo, providers.LabelReady); reason != backlogHealthScanIntegrityMismatch {
		t.Fatalf("mis-keyed gaggle reason = %q", reason)
	}
	if _, reason := readBacklogHealthCursor(path, "", repo, "other:label"); reason != backlogHealthScanIntegrityMismatch {
		t.Fatalf("mis-keyed label reason = %q", reason)
	}

	// A transition above the high-water mark contradicts the mark itself.
	contradictory := valid
	contradictory.Transitions = []providers.WorkItemLabelTransition{
		{EventID: 99, ItemID: "7", Label: providers.LabelReady, Added: true, OccurredAt: time.Now().UTC()},
	}
	if err := writeBacklogHealthCursor(path, contradictory); err != nil {
		t.Fatal(err)
	}
	if _, reason := readBacklogHealthCursor(path, "", repo, providers.LabelReady); reason != backlogHealthScanIntegrityMismatch {
		t.Fatalf("contradictory ledger reason = %q", reason)
	}
}
