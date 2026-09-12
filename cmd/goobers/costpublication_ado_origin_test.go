package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func TestCostPublicationADODelayedUsesRecordedOrigin(t *testing.T) {
	for _, name := range []string{"enabled-origin", "disabled-origin", "legacy-origin"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "ado")
			if code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root); code != 0 {
				t.Fatalf("init: %d %s", code, stderr)
			}
			on, off := mixedCostGaggles(t, root)
			set, report, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
			if err != nil {
				t.Fatalf("load: %v %+v", err, report)
			}
			project := set.Gaggles[0].Spec.Project
			repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: project.Owner, Project: project.Project, Name: project.Name}
			origin, sweep, want := on, off, 1
			if name != "enabled-origin" {
				origin, sweep, want = off, on, 0
			}
			if name == "legacy-origin" {
				origin = ""
			}
			state := &adoCostOriginServer{state: "New", receipt: costComment(t, "goobers", "implementation", "cost-run", 20, 8_000_000_000).Body}
			server := httptest.NewServer(http.HandlerFunc(state.serve))
			t.Cleanup(server.Close)
			installADOStageProvider(t, repo, server)
			t.Setenv(executor.CredentialEnvVar(string(capability.ADOPRWrite)), "ado-token")
			t.Setenv(executor.InputEnvVar(executor.InputResultFile), filepath.Join(t.TempDir(), "reconcile-result.json"))
			t.Setenv(executor.GaggleEnvVar, origin)
			if err := recordPostMergeTimeout(root, repo, "359", time.Now().Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			t.Setenv(executor.GaggleEnvVar, sweep)
			code, stdout, stderr := runArgs(t, "reconcile-post-merge", root)
			if code != 0 {
				t.Fatalf("reconcile: %d stdout=%s stderr=%s", code, stdout, stderr)
			}
			entry := loadPostMergeReconcileEntry(t, root, repo, "359")
			if entry.Gaggle != origin || entry.State != postMergeReconcileCompleted || !entry.Actions.ClosedIssueNumbers["42"] {
				t.Fatalf("origin/close-out checkpoint lost: %+v", entry)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.state != "Done" || !strings.Contains(state.tags, "goobers/status:done") || len(state.comments) != 1 || !strings.Contains(state.comments[0], "Merged in pull request #359.") {
				t.Fatalf("normal close-out missing: state=%s tags=%s comments=%v stderr=%s", state.state, state.tags, state.comments, stderr)
			}
			if state.prReads != want || state.prWrites != want || strings.Contains(state.comments[0], "AIC") != (want == 1) {
				t.Fatalf("origin=%q sweep=%q PR cost reads/writes=%d/%d want=%d/%d close-out=%s stderr=%s", origin, sweep, state.prReads, state.prWrites, want, want, state.comments[0], stderr)
			}
		})
	}
}

type adoCostOriginServer struct {
	mu                   sync.Mutex
	state, tags, receipt string
	comments             []string
	prReads, prWrites    int
}

func (s *adoCostOriginServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.ToLower(r.URL.Path)
	var response any
	switch {
	case strings.HasSuffix(path, "/connectiondata"):
		response = map[string]any{"authenticatedUser": map[string]string{"providerDisplayName": "goobers"}}
	case strings.HasSuffix(path, "/threads"):
		comment := s.receipt
		if r.Method == http.MethodPost {
			s.prWrites++
			var body struct{ Comments []struct{ Content string } }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Comments) != 1 {
				http.Error(w, "invalid thread", http.StatusBadRequest)
				return
			}
			comment = body.Comments[0].Content
		} else {
			s.prReads++
		}
		thread := map[string]any{"id": 1, "comments": []any{map[string]any{"id": 1, "content": comment, "commentType": "text", "author": map[string]string{"displayName": "goobers"}}}}
		response = thread
		if r.Method == http.MethodGet {
			response = map[string]any{"value": []any{thread}}
		}
	case strings.Contains(path, "/workitemtypes/"):
		response = map[string]any{"value": []any{map[string]string{"name": "New", "category": "Proposed"}, map[string]string{"name": "Done", "category": "Completed"}}}
	case strings.HasSuffix(path, "/workitems/42/comments"):
		response = map[string]any{"comments": []any{}}
		if r.Method == http.MethodPost {
			var body struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid comment", http.StatusBadRequest)
				return
			}
			s.comments = append(s.comments, body.Text)
			response = map[string]any{"id": 1, "text": body.Text}
		}
	case strings.HasSuffix(path, "/workitems/42"):
		if r.Method == http.MethodPatch {
			var operations []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&operations); err != nil {
				http.Error(w, "invalid patch", http.StatusBadRequest)
				return
			}
			for _, op := range operations {
				p, _ := op["path"].(string)
				v, _ := op["value"].(string)
				switch p {
				case "/fields/System.State":
					s.state = v
				case "/fields/System.Tags":
					s.tags = v
				}
			}
		}
		response = map[string]any{"id": 42, "rev": 1, "fields": map[string]any{"System.WorkItemType": "Task", "System.State": s.state, "System.Tags": s.tags, "System.Title": "work"}}
	case strings.HasSuffix(path, "/pullrequests/359"):
		response = map[string]any{"pullRequestId": 359, "status": "completed", "description": "Fixes #42",
			"repository": map[string]any{"id": "repo-guid", "project": map[string]string{"id": "project-guid"}}}
	case strings.HasSuffix(path, "/evaluations"), strings.HasSuffix(path, "/statuses"):
		response = map[string]any{"value": []any{}}
	default:
		http.Error(w, "unexpected endpoint "+path, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		return
	}
}
