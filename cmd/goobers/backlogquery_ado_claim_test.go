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

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestADOBacklogQueryReadOnlyAndClaimAgreeWithReadyLabel(t *testing.T) {
	var comments []map[string]any
	tags := "goobers:approved; goobers:ready"
	revision := 1

	mux := http.NewServeMux()
	mux.HandleFunc("/org/backlog/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"workItems": []map[string]int{{"id": 42}}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitemtypes/Issue/states", func(w http.ResponseWriter, r *http.Request) {
		writeADOJSON(t, w, map[string]any{"value": []map[string]string{{"name": "Active", "category": "InProgress"}}})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workItems/42/comments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeADOJSON(t, w, map[string]any{"comments": comments})
		case http.MethodPost:
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode comment: %v", err)
			}
			comments = append(comments, map[string]any{
				"id":   len(comments) + 1,
				"text": body["text"],
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
				t.Fatalf("decode work item patch: %v", err)
			}
			for _, operation := range patch {
				if operation["path"] == "/fields/System.Tags" {
					tags, _ = operation["value"].(string)
				}
			}
			revision++
		}
		writeADOJSON(t, w, map[string]any{
			"id": 42, "rev": revision,
			"fields": map[string]any{
				"System.WorkItemType": "Issue",
				"System.Title":        "ADO ready item",
				"System.State":        "Active",
				"System.Tags":         tags,
			},
		})
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

	var readOnlyOut, readOnlyErr bytes.Buffer
	readOnlyEnv := backlogQueryEnv{
		root:          root,
		layout:        layoutFor(root),
		repo:          repo,
		backlogRepo:   repo,
		issueProvider: provider,
		stdout:        &readOnlyOut,
		stderr:        &readOnlyErr,
	}
	if code := runBacklogQueryMode(backlogQueryModeReadOnly, readOnlyEnv, nil); code != 0 {
		t.Fatalf("ADO read-only: code=%d stdout=%q stderr=%q", code, readOnlyOut.String(), readOnlyErr.String())
	}
	if !strings.Contains(readOnlyOut.String(), "42\tADO ready item") {
		t.Fatalf("ADO read-only output = %q, want eligible item 42", readOnlyOut.String())
	}

	workDir := t.TempDir()
	t.Chdir(workDir)
	var claimOut, claimErr bytes.Buffer
	claimEnv := readOnlyEnv
	claimEnv.stdout = &claimOut
	claimEnv.stderr = &claimErr
	if code := runBacklogQueryMode(backlogQueryModeClaim, claimEnv, nil); code != 0 {
		t.Fatalf("ADO claim: code=%d stdout=%q stderr=%q", code, claimOut.String(), claimErr.String())
	}
	if !strings.Contains(claimOut.String(), "claimed 42") {
		t.Fatalf("ADO claim output = %q, want claimed item 42", claimOut.String())
	}
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed item: %v", err)
	}
	if strings.Contains(string(data), `"readyAt"`) {
		t.Fatalf("ADO claim invented unsupported ready transition time: %s", data)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if entry, ok := ledger.LookupScoped(localscheduler.ClaimKey{
		Gaggle: "example", Provider: string(providers.ProviderADO), ExternalID: "42",
	}); !ok || entry.RunID != "ado-ready-run" {
		t.Fatalf("ADO ledger claim = (%+v, %v), want run ado-ready-run", entry, ok)
	}
	if !strings.Contains(tags, providers.LabelClaimed) {
		t.Fatalf("ADO tags = %q, want visible claim marker", tags)
	}
}

// TestClaimReadyAtSupportedExcludesOnlyADO pins that dropping ReadyAt
// enrichment is scoped to the provider that cannot derive it: GitHub and Gitea
// both expose label-transition history and keep enriching (and fail-closing on
// malformed history) exactly as before.
func TestClaimReadyAtSupportedExcludesOnlyADO(t *testing.T) {
	for kind, want := range map[providers.ProviderKind]bool{
		providers.ProviderGitHub: true,
		providers.ProviderGitea:  true,
		providers.ProviderADO:    false,
	} {
		if got := claimReadyAtSupported(kind); got != want {
			t.Errorf("claimReadyAtSupported(%q) = %t, want %t", kind, got, want)
		}
	}
}
