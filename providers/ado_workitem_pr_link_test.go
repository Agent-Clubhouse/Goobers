package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestADOProviderLinkPullRequestToWorkItemIsIdempotent(t *testing.T) {
	const artifactURL = "vstfs:///Git/PullRequestId/project-guid%2Frepo-guid%2F42"
	var mu sync.Mutex
	rev := 7
	relations := []adoRelation{}
	patches := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/org/code/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId": 42,
			"repository": map[string]interface{}{
				"id": "repo-guid",
				"project": map[string]string{
					"id":   "project-guid",
					"name": "code",
				},
			},
		})
	})
	mux.HandleFunc("/org/backlog/_apis/wit/workitems/100", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("$expand") != "Relations" {
				t.Fatalf("$expand = %q, want Relations", r.URL.Query().Get("$expand"))
			}
			writeJSON(t, w, adoWorkItem{ID: 100, Rev: rev, Relations: relations})
		case http.MethodPatch:
			if got := r.Header.Get("Content-Type"); got != "application/json-patch+json" {
				t.Fatalf("Content-Type = %q", got)
			}
			var patch []adoPatchOperation
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
			if len(patch) != 2 || patch[0].Op != "test" || patch[0].Path != "/rev" || patch[1].Op != "add" || patch[1].Path != "/relations/-" {
				t.Fatalf("patch = %#v", patch)
			}
			value, ok := patch[1].Value.(map[string]interface{})
			if !ok {
				t.Fatalf("relation value = %#v", patch[1].Value)
			}
			if value["rel"] != "ArtifactLink" || value["url"] != artifactURL {
				t.Fatalf("relation value = %#v", value)
			}
			relations = append(relations, adoRelation{Rel: "ArtifactLink", URL: artifactURL})
			rev++
			patches++
			writeJSON(t, w, adoWorkItem{ID: 100, Rev: rev, Relations: relations})
		default:
			t.Fatalf("method = %s", r.Method)
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "code", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	codeRepo := RepositoryRef{Owner: "org", Project: "code", Name: "repo"}
	backlogRepo := RepositoryRef{Owner: "org", Project: "backlog", Name: "repo"}

	for i := 0; i < 2; i++ {
		if err := provider.LinkPullRequestToWorkItem(context.Background(), codeRepo, backlogRepo, "100", "42"); err != nil {
			t.Fatalf("LinkPullRequestToWorkItem call %d: %v", i+1, err)
		}
	}
	if patches != 1 {
		t.Fatalf("PATCH calls = %d, want 1", patches)
	}
}
