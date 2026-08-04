package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestCloseMootPullRequestDispatchesToADO(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		wantCode   int
		wantError  string
	}{
		{name: "success", statusCode: http.StatusOK},
		{name: "failure", statusCode: http.StatusInternalServerError, wantCode: 1, wantError: "status 500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch {
					t.Fatalf("method = %s, want PATCH", r.Method)
				}
				if test.statusCode != http.StatusOK {
					http.Error(w, "close failed", test.statusCode)
					return
				}
				_, _ = w.Write([]byte(`{"pullRequestId":42,"status":"abandoned"}`))
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) {
				p.BaseURL = server.URL
			})
			var stdout, stderr bytes.Buffer
			resultFile := filepath.Join(t.TempDir(), "verdict-result.json")
			code := closeMootPullRequest(
				context.Background(),
				provider,
				providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project", Name: "repo"},
				42,
				&providers.PullRequestSummary{Number: 42, HeadSHA: "head", BaseSHA: "base"},
				apiv1.Verdict{Rationale: "No longer needed."},
				"its diff is empty",
				resultFile,
				&stdout,
				&stderr,
			)
			if code != test.wantCode {
				t.Fatalf("code = %d, want %d; stderr = %q", code, test.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.wantError) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), test.wantError)
			}
		})
	}
}
