package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestADOPollScopeRejectsWrongIdentityBeforePolicies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*adoPullRequestDetail)
	}{
		{"wrong PR", func(pr *adoPullRequestDetail) { pr.PullRequestID = 43 }},
		{"wrong repository", func(pr *adoPullRequestDetail) { pr.Repository.Name = "foreign" }},
		{"wrong project", func(pr *adoPullRequestDetail) { pr.Repository.Project.Name = "foreign" }},
		{"missing repository ID", func(pr *adoPullRequestDetail) { pr.Repository.ID = "" }},
		{"missing project ID", func(pr *adoPullRequestDetail) { pr.Repository.Project.ID = "" }},
		{"missing lifecycle", func(pr *adoPullRequestDetail) { pr.Status = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var pr adoPullRequestDetail
				pr.PullRequestID = 42
				pr.Status = "active"
				pr.Repository = adoRepository{ID: "repo-id", Name: "repo", Project: adoProject{ID: "project-id", Name: "project"}}
				tc.mutate(&pr)
				if err := json.NewEncoder(w).Encode(pr); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			scope := RepositoryRef{Owner: "org", Project: "project", Name: "repo"}
			p := NewADOProvider("org", "project", "token", WithADOPollScope(scope), func(p *ADOProvider) { p.BaseURL = srv.URL })
			got, err := p.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: scope, PullID: "42"})
			if err == nil || got.CheckState == CheckStatePassing || calls != 1 {
				t.Fatalf("got %+v err=%v calls=%d", got, err, calls)
			}
		})
	}
}

func TestADOPollScopeRejectsWrongRequestWithoutHTTP(t *testing.T) {
	scope := RepositoryRef{Owner: "org", Project: "project", Name: "repo"}
	p := NewADOProvider("org", "project", "token", WithADOPollScope(scope), func(p *ADOProvider) {
		p.Client = adoHTTPClientFunc(func(*http.Request) (*http.Response, error) { t.Fatal("wrong scope reached HTTP"); return nil, nil })
	})
	for _, req := range []PullRequestPollRequest{
		{Repository: RepositoryRef{Owner: "other", Name: "repo"}, PullID: "42"},
		{Repository: RepositoryRef{Owner: "org", Name: "other"}, PullID: "42"},
		{Repository: RepositoryRef{Owner: "org", Project: "other", Name: "repo"}, PullID: "42"},
		{Repository: scope, PullID: "0"}, {Repository: scope, PullID: "../42"},
	} {
		if _, err := p.PollPullRequest(context.Background(), req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
}

func TestADOPolicyEvaluationsRequireAnExplicitList(t *testing.T) {
	for _, body := range []string{`{}`, `{"value":null}`, `{"value":{}}`, `null`, `{"value":[{}]}`, `{"value":[{"status":"approved","configuration":{"isEnabled":true,"type":{"displayName":"Build"}}}]}`, `{"value":[]}`} {
		t.Run(body, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", prDetailHandler(t, nil))
			mux.HandleFunc("/org/project/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			srv := httptest.NewServer(mux)
			defer srv.Close()
			p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = srv.URL })
			result, err := p.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: RepositoryRef{Name: "repo"}, PullID: "42"})
			if body == `{"value":[]}` {
				if err != nil || result.CheckState != CheckStatePassing {
					t.Fatalf("valid empty list: %+v %v", result, err)
				}
			} else if err == nil || result.CheckState == CheckStatePassing {
				t.Fatalf("incomplete response passed: %+v %v", result, err)
			}
		})
	}
}

func TestADOPollScopeAcceptsConfiguredIDs(t *testing.T) {
	scope := RepositoryRef{Owner: "org", Project: "proj-guid", Name: "repo-guid"}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/proj-guid/_apis/git/repositories/repo-guid/pullrequests/42", prDetailHandler(t, nil))
	mux.HandleFunc("/org/project/_apis/policy/evaluations", policyEvaluationsHandler(t, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := NewADOProvider("org", "proj-guid", "token", WithADOPollScope(scope), func(p *ADOProvider) { p.BaseURL = srv.URL })
	result, err := p.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: scope, PullID: "42"})
	if err != nil || result.CheckState != CheckStatePassing {
		t.Fatalf("ID route: %+v %v", result, err)
	}
}
