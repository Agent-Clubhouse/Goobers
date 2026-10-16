package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// TestReconcileBranchesDispatchesToADO proves reconcile-branches runs end to
// end on Azure DevOps (#5900): the ADO provider is built from the declared
// github:branch:delete credential, lists the namespace through the Git Refs
// API, checks for an active pull request, re-reads the exact ref with its last
// push, and deletes it with an oldObjectId lease.
func TestReconcileBranchesDispatchesToADO(t *testing.T) {
	now := time.Now().UTC()
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	branch := createBranchReconcileRun(t, layout.RunsDir(), "implementation", "ado-reconcile", now.Add(-8*24*time.Hour), true, true)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	deliverEveryADOStageCapability(t)

	const refsPath = "/acme/project/_apis/git/repositories/web/refs"
	var deleted []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == refsPath:
			if filter := r.URL.Query().Get("filter"); !strings.HasPrefix("heads/"+branch, filter) {
				t.Errorf("refs filter = %q, want a prefix of heads/%s", filter, branch)
			}
			body = map[string]any{"value": []map[string]string{
				{"name": "refs/heads/" + branch, "objectId": "branch-sha"},
			}}
		case r.Method == http.MethodGet && r.URL.Path == "/acme/project/_apis/git/repositories/web/pullrequests":
			body = map[string]any{"value": []any{}}
		case r.Method == http.MethodGet && r.URL.Path == "/acme/project/_apis/git/repositories/web/pushes":
			body = map[string]any{"value": []map[string]any{{
				"pushId": 3, "date": now.Add(-8 * 24 * time.Hour),
				"refUpdates": []map[string]string{{"name": "refs/heads/" + branch}},
			}}}
		case r.Method == http.MethodPost && r.URL.Path == refsPath:
			if err := json.NewDecoder(r.Body).Decode(&deleted); err != nil {
				t.Errorf("decode ref update: %v", err)
			}
			body = map[string]any{"value": []map[string]any{{"name": "refs/heads/" + branch, "success": true, "updateStatus": "succeeded"}}}
		default:
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	original := newADOProviderForStage
	newADOProviderForStage = func(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		if got := deliveredCapabilityOf(t, credential); got != string(capability.GitHubBranchDelete) {
			t.Fatalf("reconcile-branches built its ADO provider from %q, want %q", got, capability.GitHubBranchDelete)
		}
		provider, err := buildADOProviderForStage(routed, credential)
		if err != nil {
			return nil, err
		}
		provider.BaseURL = server.URL
		return provider, nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	code, stdout, stderr := runArgs(t, "reconcile-branches", "--delete", "--min-age=1h", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if len(deleted) != 1 || deleted[0]["name"] != "refs/heads/"+branch ||
		deleted[0]["oldObjectId"] != "branch-sha" || strings.Trim(deleted[0]["newObjectId"], "0") != "" {
		t.Fatalf("ref updates = %#v, want one leased delete of %s", deleted, branch)
	}
	events := branchReconcileEvents(t, layout.SchedulerDir())
	if len(events) != 2 || events[0].Runner["outcome"] != "delete-approved" ||
		events[0].Runner["lastActivityAt"] == "" || events[1].Runner["outcome"] != "deleted" {
		t.Fatalf("events = %+v, want an approved decision with push activity, then a deletion", events)
	}
}
