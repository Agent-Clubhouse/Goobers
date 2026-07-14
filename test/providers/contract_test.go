// Package providers_contract is the M5 cross-provider contract suite. It asserts
// that the GitHub and ADO implementations of providers.Provider exhibit IDENTICAL
// observable behavior on the unified work-item model — the parity guarantee from
// docs/requirements/backlog-providers.md (BL-001/002/011/012). Each provider has a
// backend serving API-shaped mock responses for the SAME logical work item; the
// same assertions then run against both. If the two ever diverge, this fails.
//
// This complements (does not duplicate) the per-provider unit tests in /providers:
// those test each impl's wire handling; this pins the shared contract both must meet.
package providers_contract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// backend builds a Provider wired to a mock HTTP server, plus the RepositoryRef to
// address it. closeFn tears down the server.
type backend struct {
	name     string
	provider providers.Provider
	repo     providers.RepositoryRef
	kind     providers.ProviderKind
}

// newGitHubBackend serves a single issue #7 labeled with a routing label and a
// claimed status label, with a milestone (hierarchy parent).
func newGitHubBackend(t *testing.T) (backend, func()) {
	t.Helper()
	// labels is the live label set; the status write-back now swaps only the
	// status label via the label sub-API (add/remove), so GET must reflect the
	// mutation for the re-read to observe in-progress (#140).
	labels := []string{"route/backend", "goobers/status:claimed"}
	labelObjs := func() []map[string]string {
		out := make([]map[string]string, 0, len(labels))
		for _, l := range labels {
			out = append(out, map[string]string{"name": l})
		}
		return out
	}
	issue := func() map[string]interface{} {
		return map[string]interface{}{
			"id": 123, "number": 7, "title": "Fix API", "body": "do it", "state": "open",
			"html_url":  "https://github.com/acme/app/issues/7",
			"labels":    labelObjs(),
			"assignees": []map[string]string{{"login": "mona"}},
			"milestone": map[string]interface{}{"number": 2, "title": "v1", "html_url": "https://github.com/acme/app/milestone/2"},
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{issue()})
	})
	mux.HandleFunc("/repos/acme/app/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Labels []string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode label add: %v", err)
		}
		labels = append(labels, body.Labels...)
		writeJSON(t, w, labelObjs())
	})
	mux.HandleFunc("/repos/acme/app/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(t, w, issue())
	})
	srv := httptest.NewServer(mux)
	p := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) { p.BaseURL = srv.URL })
	return backend{
		name:     "github",
		provider: p,
		repo:     providers.RepositoryRef{Owner: "acme", Name: "app"},
		kind:     providers.ProviderGitHub,
	}, srv.Close
}

// newADOBackend serves work item 42 with the same logical shape (routing label +
// claimed status via tags, a hierarchy-reverse relation as parent).
func newADOBackend(t *testing.T) (backend, func()) {
	t.Helper()
	item := func(tags string) map[string]interface{} {
		return map[string]interface{}{
			"id": 42, "rev": 3, "url": "https://dev.azure.com/org/project/_workitems/edit/42",
			"fields": map[string]interface{}{
				"System.WorkItemType": "User Story",
				"System.Title":        "Fix API",
				"System.Description":  "do it",
				"System.State":        "Active",
				"System.Tags":         tags,
				"System.AssignedTo":   map[string]interface{}{"displayName": "Mona"},
			},
			"relations": []map[string]interface{}{
				{"rel": "System.LinkTypes.Hierarchy-Reverse", "url": "https://dev.azure.com/org/_apis/wit/workItems/41"},
			},
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 42}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			writeJSON(t, w, item("route/backend; goobers/status:in-progress"))
			return
		}
		writeJSON(t, w, item("route/backend; goobers/status:claimed"))
	})
	srv := httptest.NewServer(mux)
	p := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = srv.URL })
	return backend{
		name:     "ado",
		provider: p,
		repo:     providers.RepositoryRef{Name: "repo", Project: "project"},
		kind:     providers.ProviderADO,
	}, srv.Close
}

type backendFactory struct {
	name string
	make func(*testing.T) (backend, func())
}

func allBackends() []backendFactory {
	return []backendFactory{
		{"github", newGitHubBackend},
		{"ado", newADOBackend},
	}
}

// TestContract_ListMapsToUnifiedModel: both providers must map a backend item to
// the same unified shape — claimed status, routing label preserved, hierarchy
// parent preserved (BL-002, BL-004, BL-012).
func TestContract_ListMapsToUnifiedModel(t *testing.T) {
	for _, bf := range allBackends() {
		t.Run(bf.name, func(t *testing.T) {
			b, done := bf.make(t)
			defer done()
			items, err := b.provider.ListWorkItems(context.Background(), providers.ListWorkItemsRequest{
				Repository: b.repo, Labels: []string{"route/backend"}, State: "open",
			})
			if err != nil {
				t.Fatalf("ListWorkItems: %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("len(items)=%d, want 1", len(items))
			}
			it := items[0]
			if it.Provider != b.kind {
				t.Errorf("Provider=%q, want %q", it.Provider, b.kind)
			}
			if it.Status != providers.WorkItemStatusClaimed {
				t.Errorf("Status=%q, want claimed (status-label parity)", it.Status)
			}
			if !it.HasLabel("route/backend") {
				t.Errorf("routing label not preserved: %v", it.Labels)
			}
			if it.Title != "Fix API" {
				t.Errorf("Title=%q, want %q", it.Title, "Fix API")
			}
			if it.Parent == nil {
				t.Errorf("hierarchy parent not preserved (BL-012)")
			}
		})
	}
}

// TestContract_StatusWriteBack: updating status must return an item whose Status
// reflects the new value, for both providers (BL-005 write-back).
func TestContract_StatusWriteBack(t *testing.T) {
	for _, bf := range allBackends() {
		t.Run(bf.name, func(t *testing.T) {
			b, done := bf.make(t)
			defer done()
			it, err := b.provider.UpdateWorkItemStatus(context.Background(), providers.UpdateWorkItemStatusRequest{
				Repository: b.repo, ID: idFor(b.kind), Status: providers.WorkItemStatusInProgress,
			})
			if err != nil {
				t.Fatalf("UpdateWorkItemStatus: %v", err)
			}
			if it.Status != providers.WorkItemStatusInProgress {
				t.Errorf("Status=%q, want in-progress", it.Status)
			}
		})
	}
}

// TestContract_NonZeroHTTPSurfacesError: a non-2xx backend response must surface
// as an error from both providers (no silent success).
func TestContract_NonZeroHTTPSurfacesError(t *testing.T) {
	for _, bf := range allBackends() {
		t.Run(bf.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			}))
			defer srv.Close()
			var p providers.Provider
			var repo providers.RepositoryRef
			switch bf.name {
			case "github":
				p = providers.NewGitHubProvider("t", func(p *providers.GitHubProvider) { p.BaseURL = srv.URL })
				repo = providers.RepositoryRef{Owner: "acme", Name: "app"}
			case "ado":
				p = providers.NewADOProvider("org", "project", "t", func(p *providers.ADOProvider) { p.BaseURL = srv.URL })
				repo = providers.RepositoryRef{Name: "repo", Project: "project"}
			}
			if _, err := p.GetWorkItem(context.Background(), repo, idFor(bf.kindFor())); err == nil {
				t.Fatalf("expected error on 500 response, got nil")
			}
		})
	}
}

// TestContract_WebhookSubscriptionUnsupported: both providers reject a webhook
// subscription kind (in-process delivery is polling-only; webhooks are external).
func TestContract_WebhookSubscriptionUnsupported(t *testing.T) {
	for _, bf := range allBackends() {
		t.Run(bf.name, func(t *testing.T) {
			b, done := bf.make(t)
			defer done()
			_, err := b.provider.Subscribe(context.Background(), providers.TriggerSubscription{
				Kind: providers.TriggerWebhook, Repository: b.repo,
			})
			if err == nil {
				t.Fatalf("expected webhook subscription to be unsupported, got nil error")
			}
		})
	}
}

// idFor returns the backend's known work-item id for the given provider kind.
func idFor(kind providers.ProviderKind) string {
	if kind == providers.ProviderADO {
		return "42"
	}
	return "7"
}

func (bf backendFactory) kindFor() providers.ProviderKind {
	if bf.name == "ado" {
		return providers.ProviderADO
	}
	return providers.ProviderGitHub
}

func writeJSON(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
}
