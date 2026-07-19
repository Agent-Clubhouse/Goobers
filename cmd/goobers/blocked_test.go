package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedBlockedRecords writes blocked.json directly under the instance's
// scheduler dir (bypassing the lock — this is test setup, not the code path
// under test), returning the demo root.
func seedBlockedRecords(t *testing.T, recs map[string]blockedRecord) string {
	t.Helper()
	root := initDemo(t)
	path := blockedRecordsPath(layoutFor(root))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	if err := saveBlockedRecords(path, recs); err != nil {
		t.Fatalf("seed blocked records: %v", err)
	}
	return root
}

// TestBlockedListReportsRecords is #973: `blocked list` prints every recorded
// entry, including a "pr/"-prefixed key, sorted for determinism.
func TestBlockedListReportsRecords(t *testing.T) {
	root := seedBlockedRecords(t, map[string]blockedRecord{
		"955":    {Blockers: []string{"956", "957"}, RunID: "run-a", Stage: "implement", Reason: "sibling ordering", RecordedAt: time.Unix(1, 0).UTC()},
		"pr/966": {Blockers: []string{"969"}, RunID: "run-b", Stage: "gather-pr-context", Reason: "duplicate pr", RecordedAt: time.Unix(2, 0).UTC()},
	})

	code, stdout, stderr := runArgs(t, "blocked", "list", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	for _, want := range []string{"955", "pr/966", "956", "969", "sibling ordering", "duplicate pr"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// TestBlockedListJSON is #973: --json emits the raw record map.
func TestBlockedListJSON(t *testing.T) {
	root := seedBlockedRecords(t, map[string]blockedRecord{
		"955": {Blockers: []string{"956"}, RunID: "run-a", RecordedAt: time.Unix(1, 0).UTC()},
	})

	code, stdout, stderr := runArgs(t, "blocked", "list", "--json", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got map[string]blockedRecord
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if rec, ok := got["955"]; !ok || rec.RunID != "run-a" {
		t.Fatalf("parsed records = %+v, want a 955 entry from run-a", got)
	}
}

// TestBlockedListEmpty is #973: an absent ledger is a normal, exit-0 outcome.
func TestBlockedListEmpty(t *testing.T) {
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "blocked", "list", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no blocked records") {
		t.Fatalf("stdout = %q, want a clear empty-ledger message", stdout)
	}
}

// TestBlockedClearRemovesRecord is #973: clear removes exactly the named
// entry (including a "pr/"-prefixed key) and leaves the rest intact; a second
// clear of the same id is a business error (exit 1).
func TestBlockedClearRemovesRecord(t *testing.T) {
	root := seedBlockedRecords(t, map[string]blockedRecord{
		"955":    {Blockers: []string{"956"}, RunID: "run-a", RecordedAt: time.Unix(1, 0).UTC()},
		"pr/966": {Blockers: []string{"969"}, RunID: "run-b", RecordedAt: time.Unix(2, 0).UTC()},
	})

	code, stdout, stderr := runArgs(t, "blocked", "clear", "pr/966", root)
	if code != 0 {
		t.Fatalf("clear: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "cleared blocked record pr/966") {
		t.Fatalf("stdout = %q, want a cleared confirmation", stdout)
	}

	recs, err := loadBlockedRecords(blockedRecordsPath(layoutFor(root)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, gone := recs["pr/966"]; gone {
		t.Fatalf("pr/966 still present after clear: %+v", recs)
	}
	if _, kept := recs["955"]; !kept {
		t.Fatalf("955 should be untouched by clearing pr/966: %+v", recs)
	}

	// Second clear of the same id: nothing to remove → exit 1.
	code, _, _ = runArgs(t, "blocked", "clear", "pr/966", root)
	if code != 1 {
		t.Fatalf("second clear code = %d, want 1 (no such record)", code)
	}
}

// TestBlockedClearUnknown is #973: clearing an id with no record is a
// business error, distinct from a usage error.
func TestBlockedClearUnknown(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "blocked", "clear", "404", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "no blocked record") {
		t.Fatalf("stderr = %q, want a no-such-record message", stderr)
	}
}

// TestBlockedClearUsage is #973: a missing item id is a usage error (exit 2),
// not a business error.
func TestBlockedClearUsage(t *testing.T) {
	code, _, _ := runArgs(t, "blocked", "clear")
	if code != 2 {
		t.Fatalf("code = %d, want 2 (usage)", code)
	}
}
