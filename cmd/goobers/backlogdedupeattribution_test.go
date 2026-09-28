package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// TestBacklogDedupeIgnoresAttributionFooters: issues Goobers files through a
// provider with run attribution all carry the same attribution footer (and a
// run-id footer), which used to dominate the body similarity of short issues.
// Unrelated attributed siblings must give no candidate, while a real
// duplicate is still flagged.
func TestBacklogDedupeIgnoresAttributionFooters(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	creator := server.newGitHubProvider("token")
	creator.SetAttribution(providers.Attribution{
		Gaggle: "goobers", Workflow: "decomposition", Task: "create-children", Goober: "planner", Run: "run-decompose-1",
	})
	fixtures := []struct{ title, body string }{
		{"Add a retry budget to the webhook sender", "Webhook deliveries fail permanently after one timeout."},
		{"Document the release checklist", "Operators need the tagging steps written down."},
		{"Tighten portal session expiry", "Idle sessions stay valid for a week."},
		{"Webhook sender needs a retry budget", "Webhook deliveries fail permanently after a single timeout."},
	}
	for i, fixture := range fixtures {
		item, err := creator.CreateWorkItem(t.Context(), providers.CreateWorkItemRequest{
			Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"},
			Title:      fixture.title,
			Body:       fixture.body,
			RunID:      "run-decompose-1-child-" + strconv.Itoa(i+1),
		})
		if err != nil {
			t.Fatalf("create fixture issue %d: %v", i+1, err)
		}
		if _, ok, err := providers.ParseAttribution(item.Body); err != nil || !ok {
			t.Fatalf("fixture issue %s body carries no attribution (ok=%v err=%v): %q", item.ID, ok, err, item.Body)
		}
		if !strings.Contains(item.Body, providers.RunIDFooterPrefix) {
			t.Fatalf("fixture issue %s body carries no run-id footer: %q", item.ID, item.Body)
		}
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if ok, _, err := ledger.Claim("1", "curation-run", "backlog-curation", time.Hour); err != nil || !ok {
		t.Fatalf("claim fixture item: ok=%v err=%v", ok, err)
	}
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_READ", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
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
	if len(artifact.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want only the real duplicate 1/4", artifact.Candidates)
	}
	if got := artifact.Candidates[0]; got.Older.ID != "1" || got.Newer.ID != "4" {
		t.Fatalf("candidate pair = %s/%s, want 1/4", got.Older.ID, got.Newer.ID)
	}
}

func TestDedupeAuthoredBodyRemovesProviderFooters(t *testing.T) {
	attribution := providers.Attribution{Gaggle: "goobers", Workflow: "w", Task: "t", Goober: "g", Run: "run-1"}
	stamped, err := providers.StampAttribution("Written text.\n\n---\n"+providers.RunIDFooterPrefix+"run-1-child", attribution, "issue-create")
	if err != nil {
		t.Fatalf("stamp attribution: %v", err)
	}
	quoted := "See " + providers.RunIDFooterPrefix + "run-1\nfor context."
	for name, tc := range map[string]struct{ body, want string }{
		"attribution and run-id footer": {stamped, "Written text."},
		"run-id footer only":            {"Written text.\n\n---\n" + providers.RunIDFooterPrefix + "run-1", "Written text."},
		"footer on an empty body":       {"---\n" + providers.RunIDFooterPrefix + "run-1", ""},
		"plain body":                    {"  Written text.\n", "Written text."},
		"run id quoted mid-body":        {quoted, quoted},
	} {
		if got := dedupeAuthoredBody(tc.body); got != tc.want {
			t.Errorf("%s: dedupeAuthoredBody = %q, want %q", name, got, tc.want)
		}
	}
}

// TestDuplicateSignalsMatchUnattributedBodies: the footers shared by items
// from one run add neither body similarity nor shared references; a run id
// such as "run-decompose-1" would otherwise read as the external reference
// DECOMPOSE-1 shared by every item the run filed.
func TestDuplicateSignalsMatchUnattributedBodies(t *testing.T) {
	attribution := providers.Attribution{Gaggle: "goobers", Workflow: "decomposition", Task: "create-children", Goober: "planner", Run: "run-decompose-1"}
	stamp := func(body, runID string) string {
		stamped, err := providers.StampAttribution(body+"\n\n---\n"+providers.RunIDFooterPrefix+runID, attribution, "issue-create")
		if err != nil {
			t.Fatalf("stamp attribution: %v", err)
		}
		return stamped
	}
	plainA := providers.WorkItem{ID: "1", Title: "Document the release checklist", Body: "Operators need the tagging steps written down."}
	plainB := providers.WorkItem{ID: "2", Title: "Tighten portal session expiry", Body: "Idle sessions stay valid for a week."}
	stampedA, stampedB := plainA, plainB
	stampedA.Body = stamp(plainA.Body, "run-decompose-1-child-1")
	stampedB.Body = stamp(plainB.Body, "run-decompose-1-child-2")

	want := duplicateSignals(plainA, plainB, nil)
	got := duplicateSignals(stampedA, stampedB, nil)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attributed signals = %+v, want the unattributed %+v", got, want)
	}
	if _, likely := duplicateCandidateScore(got); likely {
		t.Fatalf("unrelated attributed siblings scored as likely duplicates: %+v", got)
	}
}
