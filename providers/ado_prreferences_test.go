package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestADOPullRequestReferences(t *testing.T) {
	fullBody := strings.Repeat("description beyond list truncation\n", 200) + "Fixes #123"
	for _, tc := range []struct {
		name                      string
		detailStatus, linksStatus int
		detailBody, linksBody     string
		wantErr                   bool
		wantLinks                 []string
	}{
		{name: "full description and native links", linksBody: `{"value":[{"id":" 123 "},{"id":""},{"id":"  "},{"id":"456"}]}`, wantLinks: []string{"123", "456"}},
		{name: "no native links", linksBody: `{"value":[]}`},
		{name: "detail unavailable", detailStatus: http.StatusNotFound, wantErr: true},
		{name: "malformed detail", detailBody: `{`, wantErr: true},
		{name: "links unavailable", linksStatus: http.StatusForbidden, wantErr: true},
		{name: "malformed links", linksBody: `{`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertMethod(t, r, http.MethodGet)
				if r.URL.Query().Get("api-version") != "7.1" {
					t.Errorf("api version=%s", r.URL.RawQuery)
				}
				calls = append(calls, r.URL.Path)
				switch r.URL.Path {
				case "/org/project/_apis/git/repositories/repo/pullrequests/42":
					if tc.detailStatus != 0 {
						w.WriteHeader(tc.detailStatus)
						return
					}
					if tc.detailBody != "" {
						_, _ = w.Write([]byte(tc.detailBody))
						return
					}
					writeJSON(t, w, map[string]interface{}{"pullRequestId": 42, "description": fullBody})
				case "/org/project/_apis/git/repositories/repo/pullrequests/42/workitems":
					if tc.linksStatus != 0 {
						w.WriteHeader(tc.linksStatus)
						return
					}
					_, _ = w.Write([]byte(tc.linksBody))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			got, err := p.PullRequestReferences(context.Background(), RepositoryRef{Name: "repo"}, "42")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%t", err, tc.wantErr)
			}
			if tc.wantErr {
				if got.Body != "" || len(got.WorkItemIDs) != 0 {
					t.Fatalf("partial references returned after failure: %+v", got)
				}
			} else if got.Body != fullBody || !reflect.DeepEqual(got.WorkItemIDs, tc.wantLinks) {
				t.Fatalf("body length=%d links=%v", len(got.Body), got.WorkItemIDs)
			}
			wantCalls := 2
			if tc.detailStatus != 0 || tc.detailBody != "" {
				wantCalls = 1
			}
			if len(calls) != wantCalls {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}

func TestADOPullRequestReferencesRejectsMissingIdentity(t *testing.T) {
	p := NewADOProvider("org", "project", "token", func(p *ADOProvider) {
		p.Client = adoHTTPClientFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid request reached HTTP"); return nil, nil })
	})
	for _, tc := range []struct {
		repo   RepositoryRef
		pullID string
	}{
		{repo: RepositoryRef{}, pullID: "42"},
		{repo: RepositoryRef{Name: "repo"}},
	} {
		if _, err := p.PullRequestReferences(context.Background(), tc.repo, tc.pullID); err == nil {
			t.Fatalf("accepted missing identity: %+v", tc)
		}
	}
}
