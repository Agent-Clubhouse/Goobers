package providers

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestADOBranchObservationRequiresExactRefAcrossBoundedPages(t *testing.T) {
	calls := 0
	client := adoHTTPClientFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Query().Get("filter") != "heads/child/123" || r.URL.Query().Get("$top") != "100" {
			t.Fatal("observation widened request", r.Method, r.URL)
		}
		response := &http.Response{StatusCode: 200, Header: make(http.Header)}
		body := `{"value":[{"name":"refs/heads/child/123-other","objectId":"wrong"}]}`
		if calls == 1 {
			response.Header.Set("x-ms-continuationtoken", "next")
		} else {
			if r.URL.Query().Get("continuationToken") != "next" {
				t.Fatal("pagination changed")
			}
			body = `{"value":[{"name":"refs/heads/child/123","objectId":"exact"}]}`
		}
		response.Body = io.NopCloser(strings.NewReader(body))
		return response, nil
	})
	p := NewADOProvider("org", "project", "host-read-token", func(p *ADOProvider) { p.Client = client })
	branch, found, err := p.GetBranch(t.Context(), RepositoryRef{Name: "repo"}, "child/123")
	if err != nil || !found || branch.Name != "child/123" || branch.SHA != "exact" || calls != 2 {
		t.Fatal(branch, found, err, calls)
	}
}
func TestADOBranchObservationDistinguishesMissingAndIncomplete(t *testing.T) {
	for _, mode := range []string{"missing", "too many pages", "repeated token", "too many bytes", "provider failure"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := adoHTTPClientFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet {
					t.Fatal("unexpected mutation")
				}
				response := &http.Response{StatusCode: 200, Header: make(http.Header)}
				body := `{"value":[{"name":"refs/heads/child/123-other","objectId":"wrong"}]}`
				switch mode {
				case "too many pages":
					response.Header.Set("x-ms-continuationtoken", fmt.Sprint(calls))
				case "repeated token":
					response.Header.Set("x-ms-continuationtoken", "same")
				case "too many bytes":
					body = strings.Repeat(" ", (1<<20)+1)
				case "provider failure":
					response.StatusCode = 403
				}
				response.Body = io.NopCloser(strings.NewReader(body))
				return response, nil
			})
			p := NewADOProvider("org", "project", "host-read-token", func(p *ADOProvider) { p.Client = client }, WithADOMaxRateLimitRetries(0))
			_, found, err := p.GetBranch(t.Context(), RepositoryRef{Name: "repo"}, "child/123")
			if found || (err == nil) != (mode == "missing") || calls > 4 {
				t.Fatal(found, err, calls)
			}
		})
	}
}
