package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func TestBuildADOProviderForStagePreservesConfiguredPAT(t *testing.T) {
	root := initDemo(t)
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Repos = []instance.RepoRef{{
		Provider: "ado",
		Owner:    "org",
		Project:  "project",
		Name:     "repo",
		Token:    instance.TokenRef{Env: "ADO_BACKLOG_PAT"},
	}}
	if err := instance.WriteConfig(layoutFor(root).ConfigFile(), cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ADO_BACKLOG_PAT", "backlog-pat")
	t.Setenv("GOOBERS_CRED_PROVIDER_PR_WRITE", "wrong-pr-pat")

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:backlog-pat"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Fatalf("Authorization = %q, want configured non-PR PAT", got)
		}
		switch r.URL.Path {
		case "/org/project/_apis/wit/workitems/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 42,
				"fields": map[string]any{
					"System.WorkItemType": "Task",
					"System.Title":        "Backlog item",
					"System.State":        "Active",
				},
			})
		case "/org/project/_apis/wit/workitemtypes/Task/states":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]string{{"name": "Active", "category": "InProgress"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := providers.RepositoryRef{
		Provider: providers.ProviderADO,
		Owner:    "org",
		Project:  "project",
		Name:     "repo",
	}
	provider, err := buildADOProviderForStage(root, repo)
	if err != nil {
		t.Fatalf("build ADO provider: %v", err)
	}
	provider.BaseURL = server.URL
	if _, err := provider.GetWorkItem(context.Background(), repo, "42"); err != nil {
		t.Fatalf("get backlog work item: %v", err)
	}
}
