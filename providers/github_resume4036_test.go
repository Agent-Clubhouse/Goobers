package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// Goobers#4036: resuming a caller-paged work-item scan at an arbitrary offset
// used to shrink per_page until it divided that offset evenly, so the width
// of the resumed window was a property of the offset's factorization rather
// than of what the caller asked for. Offset 158 read a 79-record page; a
// prime offset collapsed per_page to 1. A backlog scan budgeted for 250
// candidates could therefore examine a handful of them per page — and, since
// a page shorter than its own per_page is not "capped", report the result set
// exhausted while most of it had never been read.

// serveGitHubIssuePage answers GET .../issues with an oldest-first window of
// total synthetic issues, honoring page/per_page exactly as GitHub does.
func serveGitHubIssuePage(t *testing.T, total int, seen *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		perPage, _ := strconv.Atoi(query.Get("per_page"))
		if perPage <= 0 {
			perPage = 30
		}
		page, _ := strconv.Atoi(query.Get("page"))
		if page <= 0 {
			page = 1
		}
		*seen = append(*seen, "page="+strconv.Itoa(page)+" per_page="+strconv.Itoa(perPage))
		start := min((page-1)*perPage, total)
		end := min(start+perPage, total)
		issues := make([]map[string]any, 0, end-start)
		for i := start; i < end; i++ {
			issues = append(issues, map[string]any{
				"id": i + 1, "number": i + 1, "title": "candidate", "state": "open",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issues)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestGitHubListWorkItemsResumeKeepsFullPageWidth(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cursor         string
		wantRequest    string
		wantFirstID    string
		wantCount      int
		wantNextCursor string
	}{
		// 158 is the live offset from #4036: it used to shrink per_page to
		// 79 (158 = 2 x 79), reading barely half a page.
		{"live offset", "158", "page=2 per_page=100", "159", 42, "200"},
		// A prime offset used to collapse per_page all the way to 1.
		{"prime offset", "263", "page=3 per_page=100", "264", 37, "300"},
		// An aligned offset must behave exactly as it always did.
		{"aligned offset", "100", "page=2 per_page=100", "101", 100, "200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			server := serveGitHubIssuePage(t, 600, &requests)
			provider := NewGitHubProvider("tok", func(p *GitHubProvider) { p.BaseURL = server.URL })

			pageInfo := &ListWorkItemsPageInfo{}
			items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
				Repository:  RepositoryRef{Owner: "acme", Name: "app"},
				State:       "open",
				Limit:       100,
				Cursor:      tc.cursor,
				PageInfo:    pageInfo,
				OldestFirst: true,
			})
			if err != nil {
				t.Fatalf("ListWorkItems: %v", err)
			}
			if len(requests) != 1 || requests[0] != tc.wantRequest {
				t.Fatalf("requests = %v, want a single %q: the resumed read must keep the caller's page width", requests, tc.wantRequest)
			}
			if len(items) != tc.wantCount || items[0].ID != tc.wantFirstID {
				t.Fatalf("got %d items starting at %s, want %d starting at %s", len(items), items[0].ID, tc.wantCount, tc.wantFirstID)
			}
			// CandidateCount is the caller's scan-budget charge: the records
			// before the resume offset were fetched but never inspected, so
			// charging for them would starve later pages of budget for work
			// this call never did.
			if pageInfo.CandidateCount != tc.wantCount {
				t.Fatalf("CandidateCount = %d, want %d (the skipped resume prefix is not a candidate this call inspected)",
					pageInfo.CandidateCount, tc.wantCount)
			}
			if !pageInfo.HasNext || pageInfo.NextCursor != tc.wantNextCursor {
				t.Fatalf("page info = %+v, want HasNext=true NextCursor=%q", pageInfo, tc.wantNextCursor)
			}
		})
	}
}

// TestGitHubListWorkItemsResumeBeyondResultSetReportsExhaustion is the
// negative control: an offset past the end of the result set must still
// report no next page, so the caller can tell it has run off the end rather
// than looping on an empty read.
func TestGitHubListWorkItemsResumeBeyondResultSetReportsExhaustion(t *testing.T) {
	var requests []string
	server := serveGitHubIssuePage(t, 218, &requests)
	provider := NewGitHubProvider("tok", func(p *GitHubProvider) { p.BaseURL = server.URL })

	pageInfo := &ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository:  RepositoryRef{Owner: "acme", Name: "app"},
		State:       "open",
		Limit:       100,
		Cursor:      "450",
		PageInfo:    pageInfo,
		OldestFirst: true,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v, want none past the end of a 218-item set", items)
	}
	if pageInfo.HasNext || pageInfo.CandidateCount != 0 {
		t.Fatalf("page info = %+v, want HasNext=false with no candidates", pageInfo)
	}
}
