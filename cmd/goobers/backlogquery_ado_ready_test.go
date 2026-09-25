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
	"github.com/goobers/goobers/providers"
)

// TestADOBacklogQueryClaimDerivesReadyAtFromTagHistory pins ADO-N21 end to
// end: an ADO claim whose selector requires goobers:ready reads the work
// item's update history and records when the tag was added (in any casing)
// as the claimed item's readyAt, instead of failing the claim.
func TestADOBacklogQueryClaimDerivesReadyAtFromTagHistory(t *testing.T) {
	readyAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	var comments []map[string]any
	tags := "goobers:approved; GOOBERS:READY"
	revision := 3
	workItem := func() map[string]any {
		return map[string]any{
			"id": 42, "rev": revision,
			"fields": map[string]any{
				"System.WorkItemType": "Issue",
				"System.Title":        "ADO ready item",
				"System.State":        "Active",
				"System.Tags":         tags,
			},
		}
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
	mux.HandleFunc("/org/backlog/_apis/wit/workItems/42/updates", func(w http.ResponseWriter, _ *http.Request) {
		writeADOJSON(t, w, map[string]any{"count": 2, "value": []map[string]any{
			{"id": 1, "fields": map[string]any{
				"System.Tags":        map[string]any{"newValue": "goobers:approved"},
				"System.ChangedDate": map[string]any{"newValue": readyAt.Add(-time.Hour).Format(time.RFC3339)},
			}},
			{"id": 2, "fields": map[string]any{
				"System.Tags":        map[string]any{"oldValue": "goobers:approved", "newValue": tags},
				"System.ChangedDate": map[string]any{"newValue": readyAt.Format(time.RFC3339)},
			}},
		}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workItems/42/comments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeADOJSON(t, w, map[string]any{"comments": comments})
		case http.MethodPost:
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode comment: %v", err)
			}
			comments = append(comments, map[string]any{
				"id":        len(comments) + 1,
				"text":      body["text"],
				"createdBy": map[string]any{"id": "00000000-0000-0000-0000-0000000005e1"},
			})
			writeADOJSON(t, w, comments[len(comments)-1])
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
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

	provider := providers.NewADOProvider("org", "backlog", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})
	root := initDemo(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "backlog", Name: "repo"}
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", providers.LabelReady)
	t.Setenv(executor.RunIDEnvVar, "ado-ready-run")
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.WorkflowEnvVar, "implementation")

	workDir := t.TempDir()
	t.Chdir(workDir)
	var stdout, stderr bytes.Buffer
	env := backlogQueryEnv{
		root:          root,
		layout:        layoutFor(root),
		repo:          repo,
		backlogRepo:   repo,
		issueProvider: provider,
		stdout:        &stdout,
		stderr:        &stderr,
	}
	if code := runBacklogQueryMode(backlogQueryModeClaim, env, nil); code != 0 {
		t.Fatalf("ADO claim: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed item: %v", err)
	}
	var claimed struct {
		ID      string     `json:"id"`
		ReadyAt *time.Time `json:"readyAt"`
	}
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatalf("decode claimed item: %v\n%s", err, data)
	}
	if claimed.ID != "42" || claimed.ReadyAt == nil || !claimed.ReadyAt.Equal(readyAt) {
		t.Fatalf("claimed item = %s, want item 42 with readyAt %s", data, readyAt.Format(time.RFC3339))
	}
	if !strings.Contains(strings.ToLower(tags), providers.LabelClaimed) {
		t.Fatalf("ADO tags = %q, want visible claim marker", tags)
	}
}
