package branchretention

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/instanceannotations"
	"github.com/goobers/goobers/providers"
)

func TestRetentionADORejectsUnknownOrChangedDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unverified retention destination was read: %s", r.URL)
		http.Error(w, "unexpected read", http.StatusForbidden)
	}))
	defer server.Close()
	reader := ItemReader{UnknownRepository: errors.New("unknown ownership"), NewProvider: func(string, providers.RepositoryRef) (providers.Provider, error) {
		provider := providers.NewADOProvider("current-org", "current-project", "test-token")
		provider.BaseURL = server.URL
		return provider, nil
	}}
	for _, repo := range []providers.RepositoryRef{
		{Provider: providers.ProviderADO, Owner: "previous-org", Project: "project", Name: "repo"},
		{Provider: providers.ProviderADO, Project: "project", Name: "repo"},
		{Provider: providers.ProviderADO, Owner: "current-org", Name: "repo"},
	} {
		for _, kind := range []string{"issue", "pull_request"} {
			parked, err := reader.Parked(context.Background(), t.TempDir(), "17", instanceannotations.ItemRepository{Repository: repo, Kind: kind})
			if err == nil || parked {
				t.Fatalf("unknown/mismatched destination authorized: repo=%+v kind=%s parked=%v err=%v", repo, kind, parked, err)
			}
		}
	}
}

func TestRetentionADOReadsCurrentPullRequestLabels(t *testing.T) {
	for _, label := range []string{"goobers:needs-human", "goobers:escalated", "goobers:blocked", "", "read-error"} {
		t.Run(label, func(t *testing.T) {
			var labelReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("retention attempted provider mutation: %s", r.Method)
					http.Error(w, "read only", http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/org/project/_apis/git/repositories/repo/pullrequests/17":
					_, _ = fmt.Fprint(w, `{"pullRequestId":17,"status":"active","repository":{"name":"repo","project":{"id":"project-id","name":"project"}}}`)
				case "/org/project/_apis/policy/evaluations":
					_, _ = fmt.Fprint(w, `{"value":[]}`)
				case "/org/project/_apis/git/repositories/repo/pullrequests/17/labels":
					labelReads.Add(1)
					if label == "read-error" {
						http.Error(w, "labels unavailable", http.StatusForbidden)
						return
					}
					_, _ = fmt.Fprintf(w, `{"value":[{"id":"label-id","name":%q}]}`, label)
				default:
					t.Errorf("unexpected retention read: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
			reader := ItemReader{UnknownRepository: errors.New("unknown ownership"), NewProvider: func(_ string, got providers.RepositoryRef) (providers.Provider, error) {
				if got != repo {
					t.Fatal("repository changed")
				}
				provider := providers.NewADOProvider("org", "project", "test-token")
				provider.BaseURL = server.URL
				return provider, nil
			}}
			parked, err := reader.Parked(context.Background(), t.TempDir(), "17", instanceannotations.ItemRepository{Repository: repo, Kind: "pull_request"})
			if label == "read-error" {
				if err == nil {
					t.Fatal("unavailable labels authorized retention")
				}
			} else if err != nil || parked != (label != "") {
				t.Fatalf("parked=%v err=%v for label %q", parked, err, label)
			}
			if labelReads.Load() != 1 {
				t.Fatalf("current label reads=%d, want 1", labelReads.Load())
			}
		})
	}
}
