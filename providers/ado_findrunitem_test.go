package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type adoRunItemFake struct {
	fullText    []int
	recent      []int
	descs       map[int]string
	fullTextHit int
	recentHit   int
}

func (f *adoRunItemFake) provider(t *testing.T) (*ADOProvider, func()) {
	t.Helper()
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		decodeJSON(t, r, &body)
		ids := f.recent
		if strings.Contains(body.Query, "CONTAINS WORDS") {
			f.fullTextHit++
			ids = f.fullText
		} else {
			f.recentHit++
		}
		refs := make([]map[string]int, 0, len(ids))
		for _, id := range ids {
			refs = append(refs, map[string]int{"id": id})
		}
		writeJSON(t, w, map[string]interface{}{"workItems": refs})
	})
	for id, desc := range f.descs {
		mux.HandleFunc("/org/project/_apis/wit/workitems/"+strconv.Itoa(id), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]interface{}{
				"id": id, "rev": 1,
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.State":        "New",
					"System.Description":  desc,
				},
			})
		})
	}
	server := httptest.NewServer(withADOTestWorkItemsBatch(t, mux))
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL }), server.Close
}

func TestADOFindRunItem(t *testing.T) {
	repo := RepositoryRef{Name: "repo", Project: "project"}
	footer := runFooter("run-1")

	t.Run("full text hit skips recency query", func(t *testing.T) {
		f := &adoRunItemFake{fullText: []int{1}, descs: map[int]string{1: "body\n" + footer}}
		p, closeFn := f.provider(t)
		defer closeFn()
		item, found, err := p.findRunItem(context.Background(), repo, "run-1")
		if err != nil || !found {
			t.Fatalf("findRunItem = found %v, err %v; want found", found, err)
		}
		if item.ID != "1" {
			t.Fatalf("item ID = %q, want 1", item.ID)
		}
		if f.recentHit != 0 {
			t.Fatalf("recency query ran %d times, want 0", f.recentHit)
		}
	})

	t.Run("recency fallback hit", func(t *testing.T) {
		f := &adoRunItemFake{
			recent: []int{3, 2},
			descs:  map[int]string{3: "unrelated", 2: "body\n" + footer},
		}
		p, closeFn := f.provider(t)
		defer closeFn()
		item, found, err := p.findRunItem(context.Background(), repo, "run-1")
		if err != nil || !found {
			t.Fatalf("findRunItem = found %v, err %v; want found", found, err)
		}
		if item.ID != "2" {
			t.Fatalf("item ID = %q, want 2", item.ID)
		}
		if f.fullTextHit != 1 || f.recentHit != 1 {
			t.Fatalf("queries full-text=%d recent=%d, want 1 each", f.fullTextHit, f.recentHit)
		}
	})

	t.Run("no match", func(t *testing.T) {
		f := &adoRunItemFake{recent: []int{4}, descs: map[int]string{4: "other\n" + runFooter("run-2")}}
		p, closeFn := f.provider(t)
		defer closeFn()
		_, found, err := p.findRunItem(context.Background(), repo, "run-1")
		if err != nil || found {
			t.Fatalf("findRunItem = found %v, err %v; want not found", found, err)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusBadRequest)
		}))
		defer server.Close()
		p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
		if _, found, err := p.findRunItem(context.Background(), repo, "run-1"); err == nil || found {
			t.Fatalf("findRunItem = found %v, err %v; want error", found, err)
		}
	})
}
