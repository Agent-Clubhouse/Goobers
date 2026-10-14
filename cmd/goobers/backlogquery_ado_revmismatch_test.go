package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// TestADOBacklogQueryRevisionMismatchSkipsLoudly pins #5960's diagnostic: an
// ADO item whose update history ends before its current revision is skipped
// fail-closed, as any incomplete history is, but with a stable error code on
// stderr and a journal annotation. If ADO ever broke the one-update-per-
// revision assumption, every claim would fail this way, and that must be
// visible at once rather than read as one more malformed item.
func TestADOBacklogQueryRevisionMismatchSkipsLoudly(t *testing.T) {
	tags := "goobers:approved; goobers:ready"
	revision := 3
	workItem := func() map[string]any {
		return map[string]any{"id": 42, "rev": revision, "fields": map[string]any{
			"System.WorkItemType": "Issue",
			"System.Title":        "ADO ready item",
			"System.Description":  "Implement ready item.",
			"System.State":        "Active",
			"System.Tags":         tags,
		}}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeADOJSON(t, w, map[string]any{"authenticatedUser": map[string]any{
			"id": "00000000-0000-0000-0000-0000000005e1", "providerDisplayName": "Goobers Bot",
		}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/wiql", func(w http.ResponseWriter, _ *http.Request) {
		writeADOJSON(t, w, map[string]any{"workItems": []map[string]int{{"id": 42}}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitemsbatch", func(w http.ResponseWriter, _ *http.Request) {
		writeADOJSON(t, w, map[string]any{"count": 1, "value": []map[string]any{workItem()}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitemtypes/Issue/states", func(w http.ResponseWriter, _ *http.Request) {
		writeADOJSON(t, w, map[string]any{"value": []map[string]string{{"name": "Active", "category": "InProgress"}}})
	})
	// The history stops at revision 1 while the item is at 3 or later.
	mux.HandleFunc("/org/backlog/_apis/wit/workItems/42/updates", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$skip") != "0" {
			writeADOJSON(t, w, map[string]any{"count": 0, "value": []any{}})
			return
		}
		writeADOJSON(t, w, map[string]any{"count": 1, "value": []map[string]any{
			{"id": 1, "rev": 1, "fields": map[string]any{
				"System.Tags":        map[string]any{"newValue": tags},
				"System.ChangedDate": map[string]any{"newValue": time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC).Format(time.RFC3339)},
			}},
		}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workItems/42/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeADOJSON(t, w, map[string]any{"id": 1, "text": "", "createdBy": map[string]any{"id": "00000000-0000-0000-0000-0000000005e1"}})
			return
		}
		writeADOJSON(t, w, map[string]any{"comments": []any{}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			var patch []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Errorf("decode work item patch: %v", err)
			}
			for _, operation := range patch {
				if operation["path"] == "/fields/System.Tags" {
					tags, _ = operation["value"].(string)
				}
			}
			revision++
		}
		writeADOJSON(t, w, workItem())
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	root := initDemo(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "backlog", Name: "repo"}
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", providers.LabelReady)
	t.Setenv(executor.RunIDEnvVar, "ado-revmismatch-run")
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.WorkflowEnvVar, "implementation")
	workDir := t.TempDir()
	t.Chdir(workDir)
	var stdout, stderr bytes.Buffer
	env := backlogQueryEnv{
		root:        root,
		layout:      layoutFor(root),
		repo:        repo,
		backlogRepo: repo,
		issueProvider: providers.NewADOProvider("org", "backlog", "token", func(p *providers.ADOProvider) {
			p.BaseURL = server.URL
		}),
		stdout: &stdout,
		stderr: &stderr,
	}
	code := runBacklogQueryMode(backlogQueryModeClaim, env, nil)
	if code != 0 {
		t.Fatalf("ADO claim: code=%d stdout=%q stderr=%q; want the item skipped, not the stage failed", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "error: ["+providers.ErrorCodeLabelHistoryRevisionMismatch+"] skipping eligible item 42") {
		t.Fatalf("stderr = %q, want the distinct revision-mismatch error code", stderr.String())
	}
	if strings.Contains(stdout.String(), "claimed 42") || !strings.Contains(stdout.String(), "no work") {
		t.Fatalf("stdout = %q, want no work (the mismatched item skipped, not claimed)", stdout.String())
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json")); err == nil {
		var claimed struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &claimed) == nil && claimed.ID == "42" {
			t.Fatalf("claimed-item.json = %s, want item 42 not claimed", data)
		}
	}
	events, err := journal.ReadInstanceLog(layoutFor(root).SchedulerDir())
	if err != nil {
		t.Fatalf("read instance journal: %v", err)
	}
	found := false
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["annotation"] == adoRevisionMismatchAnnotation {
			found = event.Runner["itemId"] == "42" && event.Reason == providers.ErrorCodeLabelHistoryRevisionMismatch
		}
	}
	if !found {
		t.Fatalf("instance journal has no %s annotation for item 42", adoRevisionMismatchAnnotation)
	}
}
