//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
	"sigs.k8s.io/yaml"
)

func TestIntegrationContainedParentDelegatesADOChildPublicationThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	for _, mode := range []string{"publication", "publication-lost-reply"} {
		t.Run(mode, func(t *testing.T) { qualifyContainedParentJourney(t, mode, "ado") })
	}
}

func configureADOParentPublication(t *testing.T, root string, doc map[string]any) {
	t.Helper()
	t.Setenv("QUALIFICATION_REPOSITORY_TOKEN", "host-only-publication-repository")
	doc["repos"] = []any{map[string]any{"provider": "ado", "owner": "your-org", "project": "child-project", "name": "your-repo", "token": map[string]any{"env": "QUALIFICATION_REPOSITORY_TOKEN"}}}
	path := filepath.Join(root, "config/gaggles/example/gaggle.yaml")
	var gaggle map[string]any
	if err := yaml.Unmarshal([]byte(readFileContent(t, path)), &gaggle); err != nil {
		t.Fatal(err)
	}
	spec := gaggle["spec"].(map[string]any)
	spec["project"] = map[string]any{"provider": "ado", "owner": "your-org", "project": "child-project", "name": "your-repo", "branch": "main"}
	spec["backlog"] = map[string]any{"provider": "ado", "project": "your-org/child-project"}
	data, err := yaml.Marshal(gaggle)
	if err != nil {
		t.Fatal(err)
	}
	writeFileContent(t, path, string(data))
}

func (p *parentPublicationQualification) expectedPRURL() string {
	if p.ado {
		return "https://dev.azure.com/your-org/child-project/_git/your-repo/pullrequest/7"
	}
	return "https://github.com/your-org/your-repo/pull/7"
}

// Called under p.mu, just like the GitHub fixture. The provider effect is
// counted before dropping its response, so any duplicate mutation is visible.
func (p *parentPublicationQualification) serveADO(t *testing.T, w http.ResponseWriter, req *http.Request) {
	t.Helper()
	_, password, basic := req.BasicAuth()
	if !basic || password != "host-only-publication-provider:pr:write" || req.URL.Path != "/your-org/child-project/_apis/git/repositories/your-repo/pullrequests" || req.URL.Query().Get("api-version") != "7.1" {
		t.Error("ADO publication escaped its credential, project or repository boundary")
		http.Error(w, "unexpected request", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case http.MethodGet:
		if req.URL.Query().Get("searchCriteria.targetRefName") != "refs/heads/main" || req.URL.Query().Get("searchCriteria.status") != "active" {
			t.Error("ADO lookup changed its admitted base or state")
		}
		_, _ = w.Write([]byte(`{"count":0,"value":[]}`))
	case http.MethodPost:
		var body struct {
			SourceRefName, TargetRefName, Description string
			IsDraft                                   bool
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.SourceRefName != "refs/heads/"+p.head || body.TargetRefName != "refs/heads/main" || !body.IsDraft || !strings.Contains(body.Description, p.child) {
			t.Error("ADO publication changed its admitted request")
			http.Error(w, "invalid publication", http.StatusBadRequest)
			return
		}
		p.creates++
		if p.lostReply {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pullRequestId": 7, "isDraft": true, "sourceRefName": body.SourceRefName, "targetRefName": body.TargetRefName, "repository": map[string]any{"name": "your-repo", "project": map[string]any{"name": "child-project"}}, "_links": map[string]any{"web": map[string]any{"href": p.expectedPRURL()}}})
	default:
		t.Error("ADO publication attempted an unexpected mutation")
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	}
}
