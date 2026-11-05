package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestADOListPullRequestsIdentityFiltersIgnoreCase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{{
			"pullRequestId": 42,
			"createdBy":     map[string]string{"uniqueName": "Author@Example.COM"},
			"reviewers": []map[string]interface{}{
				{"uniqueName": "Reviewer@Example.COM", "vote": 0},
				{"uniqueName": "Done@Example.COM", "vote": 10},
			},
		}}})
	}))
	defer server.Close()
	p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	for _, tc := range []struct {
		name, author, reviewer string
		want                   int
	}{
		{"unfiltered", "", "", 1},
		{"author", "author@example.com", "", 1},
		{"reviewer", "", "reviewer@example.com", 1},
		{"both", "AUTHOR@example.com", "REVIEWER@example.com", 1},
		{"other author", "other@example.com", "", 0},
		{"other reviewer", "", "other@example.com", 0},
		{"already reviewed", "", "done@example.com", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ListPullRequests(context.Background(), ListPullRequestsRequest{Repository: RepositoryRef{Name: "repo"}, Author: tc.author, RequestedReviewer: tc.reviewer, SkipCheckState: true})
			if err != nil || len(got) != tc.want {
				t.Fatalf("got=%+v err=%v", got, err)
			}
			if len(got) > 0 && (got[0].Author != "Author@Example.COM" || len(got[0].RequestedReviewers) != 1 || got[0].RequestedReviewers[0] != "Reviewer@Example.COM") {
				t.Fatalf("identity spelling changed: %+v", got[0])
			}
		})
	}
}
