// Regression coverage for issue #140: provider mutations must be idempotent
// and traceable. These tests pin three behaviours that were previously wrong:
//   - CreateWorkItem re-filed a duplicate issue when a policy retry ran after
//     the original POST had already committed server-side (a timeout).
//   - UpdateWorkItemStatus rewrote the whole label set, clobbering a label a
//     human or the curator added between the read and the write.
//   - status/branch mutations left no external-ref breadcrumb for the journal.
package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateWorkItemIdempotentOnRetry: when an item already carries this run's
// footer (a prior attempt committed before its response arrived), CreateWorkItem
// must return that item instead of POSTing a duplicate.
func TestCreateWorkItemIdempotentOnRetry(t *testing.T) {
	var postCount int
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		// The search index already has the item filed by the first attempt.
		writeJSON(t, w, map[string]interface{}{
			"items": []map[string]interface{}{{
				"id": 55, "number": 55, "title": "Investigate flake",
				"state":    "open",
				"html_url": "https://github.com/acme/app/issues/55",
				"body":     "do it\n\n---\n" + runFooter("run-dup"),
				"labels":   []map[string]string{{"name": "route/backend"}},
			}},
		})
	})
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		postCount++
		t.Fatalf("CreateWorkItem POSTed a duplicate issue despite an existing run footer (retry not idempotent)")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	item, err := provider.CreateWorkItem(context.Background(), CreateWorkItemRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "Investigate flake", Body: "do it", RunID: "run-dup",
	})
	if err != nil {
		t.Fatalf("CreateWorkItem returned error: %v", err)
	}
	if item.ID != "55" {
		t.Fatalf("expected the existing item #55, got %#v", item)
	}
	if postCount != 0 {
		t.Fatalf("expected no create POST, got %d", postCount)
	}
}

// TestCreateWorkItemCreatesWhenNoRunItemExists: with no prior item, CreateWorkItem
// stamps the run footer into the body, POSTs, and records a create ref carrying
// the RunID.
func TestCreateWorkItemCreatesWhenNoRunItemExists(t *testing.T) {
	var createdBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"items": []map[string]interface{}{}})
	})
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var body struct {
			Body string `json:"body"`
		}
		decodeJSON(t, r, &body)
		createdBody = body.Body
		writeJSON(t, w, map[string]interface{}{
			"id": 77, "number": 77, "title": "New", "state": "open",
			"html_url": "https://github.com/acme/app/issues/77",
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	rec := &recordingRecorder{}
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(rec))
	item, err := provider.CreateWorkItem(context.Background(), CreateWorkItemRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "New", Body: "do it", RunID: "run-new",
	})
	if err != nil {
		t.Fatalf("CreateWorkItem returned error: %v", err)
	}
	if item.ID != "77" {
		t.Fatalf("created item = %#v", item)
	}
	if !strings.Contains(createdBody, runFooter("run-new")) {
		t.Fatalf("created body missing run footer: %q", createdBody)
	}
	ref, ok := rec.last()
	if !ok || ref.Operation != "create" || ref.RunID != "run-new" {
		t.Fatalf("expected a create ref carrying the run id, got %#v (ok=%v)", ref, ok)
	}
}

// TestUpdateWorkItemStatusPreservesForeignLabels is the core #140 item-3
// regression: a status update must swap only the status label, leaving labels a
// human or the curator added untouched — and must never PATCH the whole set.
func TestUpdateWorkItemStatusPreservesForeignLabels(t *testing.T) {
	labels := []string{"route/backend", "area/api", "goobers/status:claimed"}
	labelObjs := func() []map[string]string {
		out := make([]map[string]string, 0, len(labels))
		for _, l := range labels {
			out = append(out, map[string]string{"name": l})
		}
		return out
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var body struct {
			Labels []string `json:"labels"`
		}
		decodeJSON(t, r, &body)
		labels = append(labels, body.Labels...)
		writeJSON(t, w, labelObjs())
	})
	mux.HandleFunc("/repos/acme/app/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodDelete)
		name := strings.TrimPrefix(r.URL.Path, "/repos/acme/app/issues/7/labels/")
		var kept []string
		for _, l := range labels {
			if l != name {
				kept = append(kept, l)
			}
		}
		labels = kept
		writeJSON(t, w, labelObjs())
	})
	mux.HandleFunc("/repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			t.Fatalf("a non-terminal status update must not PATCH the issue (would race the label set)")
		}
		writeJSON(t, w, map[string]interface{}{
			"id": 7, "number": 7, "title": "Fix", "state": "open", "labels": labelObjs(),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	rec := &recordingRecorder{}
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(rec))
	item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, ID: "7", Status: WorkItemStatusInProgress,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus returned error: %v", err)
	}
	if item.Status != WorkItemStatusInProgress {
		t.Fatalf("item status = %q", item.Status)
	}
	if strings.Join(labels, ",") != "route/backend,area/api,goobers/status:in-progress" {
		t.Fatalf("labels after update = %#v; foreign labels must survive and only the status label swaps", labels)
	}
	if ref, ok := rec.last(); !ok || ref.Operation != "status" {
		t.Fatalf("expected a status ref, got %#v (ok=%v)", ref, ok)
	}
}
