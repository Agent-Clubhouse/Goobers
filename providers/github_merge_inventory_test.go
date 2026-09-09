package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGitHubMergeInventoryUsesActualMergerAndMergeTime(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, merger := range []string{"actual-merger", ""} {
		t.Run("merger="+merger, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				assertMethod(t, r, http.MethodGet)
				switch r.URL.Path {
				case "/service/repos/acme/app/pulls":
					if r.URL.Query().Get("sort") != "updated" || r.URL.Query().Get("direction") != "desc" || r.URL.Query().Get("state") != "closed" {
						t.Errorf("incorrect inventory query: %s", r.URL)
					}
					writeJSON(t, w, []map[string]any{
						{"number": 9, "updated_at": start.Add(48 * time.Hour), "merged_at": start},
						{"number": 10, "updated_at": start.Add(48 * time.Hour), "merged_at": start.Add(-time.Hour)},
						{"number": 11, "updated_at": start.Add(48 * time.Hour), "merged_at": start.Add(24 * time.Hour)},
						{"number": 12, "updated_at": start.Add(48 * time.Hour)},
					})
				case "/service/repos/acme/app/pulls/9":
					writeJSON(t, w, map[string]any{"number": 9, "merged": true, "merged_at": start, "merge_commit_sha": "commit", "merged_by": map[string]string{"login": merger}, "user": map[string]string{"login": "not-the-merger"}})
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			p := NewGitHubProvider("", func(p *GitHubProvider) { p.BaseURL = server.URL + "/service" })
			entries, err := p.MergeInventory(context.Background(), MergeInventoryRequest{Repository: RepositoryRef{Owner: "Acme", Name: "App"}, Since: start, Until: start.Add(24 * time.Hour), Limit: 4})
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries=%+v err=%v", entries, err)
			}
			entry := entries[0]
			if entry.MergedBy != merger || !entry.MergedAt.Equal(start) || entry.PullID != "9" || entry.RepositoryAPIURL != server.URL+"/service/repos/acme/app" || requests != 2 {
				t.Fatalf("incorrect provenance or unrelated reads: %+v requests=%d", entry, requests)
			}
		})
	}
}

func TestGitHubMergeInventoryNeverReturnsPartialTotals(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, mode := range []string{"bound", "duplicate", "detail mismatch", "detail failure", "missing update time", "null page"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/acme/app/pulls" {
					if mode == "null page" {
						writeJSON(t, w, nil)
						return
					}
					record := map[string]any{"number": 9, "merged_at": start, "updated_at": start}
					records := []map[string]any{record}
					switch mode {
					case "bound":
						// A recently updated but long-ago closed PR consumes
						// the raw scan bound even though it isn't a merge.
						delete(record, "merged_at")
						w.Header().Set("Link", "<http://"+r.Host+"/repos/acme/app/pulls?page=2>; rel=\"next\"")
					case "duplicate":
						records = append(records, record)
					case "missing update time":
						delete(record, "updated_at")
					}
					writeJSON(t, w, records)
					return
				}
				if mode == "detail failure" {
					http.NotFound(w, r)
					return
				}
				mergedAt := start
				if mode == "detail mismatch" {
					mergedAt = start.Add(time.Hour)
				}
				writeJSON(t, w, map[string]any{"number": 9, "merged": true, "merged_at": mergedAt})
			}))
			defer server.Close()
			p := NewGitHubProvider("", func(p *GitHubProvider) { p.BaseURL = server.URL })
			limit := 2
			if mode == "bound" {
				limit = 1
			}
			entries, err := p.MergeInventory(context.Background(), MergeInventoryRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, Since: start, Until: start.Add(time.Hour), Limit: limit})
			if err == nil || entries != nil {
				t.Fatalf("partial or uncertain inventory accepted: %+v, %v", entries, err)
			}
		})
	}
}
