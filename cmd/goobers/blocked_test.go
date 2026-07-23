package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
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

// TestBlockedListReportsRecords covers #1169's presentation contract, including
// scoped keys written before itemId was stored alongside the map key.
func TestBlockedListReportsRecords(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	root := seedBlockedRecords(t, map[string]blockedRecord{
		blockedRecordKey(repo, "102"):     {Repository: repo, Blockers: []string{"148", "144"}},
		blockedRecordKey(repo, "pr/1058"): {Repository: repo, Blockers: []string{"1076", "1044"}},
	})

	code, stdout, stderr := runArgs(t, "blocked", "list", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	want := "#102 blocked by #144, #148\nPR #1058 blocked by #1044, #1076\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestBlockedListQualifiesMultipleRepositories(t *testing.T) {
	apiRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	webRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	root := seedBlockedRecords(t, map[string]blockedRecord{
		blockedRecordKey(webRepo, "pr/1058"): {
			Repository: webRepo,
			ItemID:     "pr/1058",
			Blockers:   []string{"1044"},
		},
		blockedRecordKey(apiRepo, "102"): {
			Repository: apiRepo,
			ItemID:     "102",
			Blockers:   []string{"148"},
		},
	})

	code, stdout, stderr := runArgs(t, "blocked", "list", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	want := "acme/api#102 blocked by acme/api#148\nPR acme/web#1058 blocked by acme/web#1044\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// TestBlockedListJSON pins both record ordering and struct field ordering.
func TestBlockedListJSON(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	root := seedBlockedRecords(t, map[string]blockedRecord{
		blockedRecordKey(repo, "pr/1058"): {Repository: repo, Blockers: []string{"1076", "1044"}},
		blockedRecordKey(repo, "102"):     {Repository: repo, Blockers: []string{"148", "144"}},
	})

	code, stdout, stderr := runArgs(t, "blocked", "list", "--json", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got []blockedListRecord
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	want := `[
  {
    "ref": "#102",
    "kind": "issue",
    "blockedBy": [
      {
        "ref": "#144",
        "kind": "issue"
      },
      {
        "ref": "#148",
        "kind": "issue"
      }
    ]
  },
  {
    "ref": "#1058",
    "kind": "pull_request",
    "blockedBy": [
      {
        "ref": "#1044",
        "kind": "issue"
      },
      {
        "ref": "#1076",
        "kind": "issue"
      }
    ]
  }
]
`
	if stdout != want {
		t.Fatalf("stdout = %q, want stable JSON %q", stdout, want)
	}
}

func TestBlockedListOrdersMixedReferences(t *testing.T) {
	for _, input := range [][]string{
		{"2", "10", "1a"},
		{"2", "1a", "10"},
		{"10", "2", "1a"},
		{"10", "1a", "2"},
		{"1a", "2", "10"},
		{"1a", "10", "2"},
	} {
		sort.Slice(input, func(i, j int) bool {
			return blockedNumberLess(input[i], input[j])
		})
		if got := strings.Join(input, ","); got != "2,10,1a" {
			t.Fatalf("mixed reference order = %q, want %q", got, "2,10,1a")
		}
	}

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	records := blockedListRecords(map[string]blockedRecord{
		"record-10": {Repository: repo, ItemID: "10", Blockers: []string{"10", "1a", "2"}},
		"record-1a": {Repository: repo, ItemID: "1a"},
		"record-2":  {Repository: repo, ItemID: "2"},
	})
	if len(records) != 3 {
		t.Fatalf("record count = %d, want 3", len(records))
	}
	if got := records[0].Ref + "," + records[1].Ref + "," + records[2].Ref; got != "#2,#10,#1a" {
		t.Fatalf("record order = %q, want %q", got, "#2,#10,#1a")
	}
	blockedBy := records[1].BlockedBy
	if len(blockedBy) != 3 {
		t.Fatalf("blockedBy count = %d, want 3", len(blockedBy))
	}
	if got := blockedBy[0].Ref + "," + blockedBy[1].Ref + "," + blockedBy[2].Ref; got != "#2,#10,#1a" {
		t.Fatalf("blockedBy order = %q, want %q", got, "#2,#10,#1a")
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

func TestBlockedClearResolvesScopedKeyAndRejectsAmbiguousID(t *testing.T) {
	webRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	apiRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	webKey := blockedRecordKey(webRepo, "955")
	apiKey := blockedRecordKey(apiRepo, "955")
	root := seedBlockedRecords(t, map[string]blockedRecord{
		webKey: {Repository: webRepo, ItemID: "955", Blockers: []string{"956"}, RunID: "web-run"},
		apiKey: {Repository: apiRepo, ItemID: "955", Blockers: []string{"957"}, RunID: "api-run"},
	})

	code, _, stderr := runArgs(t, "blocked", "clear", "955", root)
	if code != 1 || !strings.Contains(stderr, "multiple blocked records") {
		t.Fatalf("ambiguous clear: code = %d, stderr = %q", code, stderr)
	}

	code, stdout, stderr := runArgs(t, "blocked", "clear", webKey, root)
	if code != 0 {
		t.Fatalf("scoped clear: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	recs, err := loadBlockedRecords(blockedRecordsPath(layoutFor(root)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := recs[webKey]; ok {
		t.Fatalf("web record still present: %+v", recs)
	}
	if _, ok := recs[apiKey]; !ok {
		t.Fatalf("API record was removed with web record: %+v", recs)
	}
}

func TestBlockedClearResolvesQualifiedDisplayRef(t *testing.T) {
	webRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	apiRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	webKey := blockedRecordKey(webRepo, "955")
	apiKey := blockedRecordKey(apiRepo, "955")
	root := seedBlockedRecords(t, map[string]blockedRecord{
		webKey: {Repository: webRepo, ItemID: "955", Blockers: []string{"956"}},
		apiKey: {Repository: apiRepo, ItemID: "955", Blockers: []string{"957"}},
	})

	code, stdout, stderr := runArgs(t, "blocked", "clear", "acme/web#955", root)
	if code != 0 {
		t.Fatalf("clear: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	recs, err := loadBlockedRecords(blockedRecordsPath(layoutFor(root)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := recs[webKey]; ok {
		t.Fatalf("web record still present: %+v", recs)
	}
	if _, ok := recs[apiKey]; !ok {
		t.Fatalf("API record was removed: %+v", recs)
	}
}

func TestBlockedClearResolvesUniqueScopedItemID(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	key := blockedRecordKey(repo, "pr/966")
	root := seedBlockedRecords(t, map[string]blockedRecord{
		key: {Repository: repo, ItemID: "pr/966", Blockers: []string{"969"}, RunID: "run-b"},
	})

	code, stdout, stderr := runArgs(t, "blocked", "clear", "pr/966", root)
	if code != 0 {
		t.Fatalf("clear: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	recs, err := loadBlockedRecords(blockedRecordsPath(layoutFor(root)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("records after clear = %+v, want empty", recs)
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
