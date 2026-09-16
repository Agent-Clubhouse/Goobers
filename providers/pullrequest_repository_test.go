package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestForgePollAndListPreserveRepositoryIdentity(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		for _, sourceOwner := range []string{"org", "contributor"} {
			t.Run(string(kind)+"/"+sourceOwner, func(t *testing.T) {
				sourceID := "123"
				if sourceOwner != "org" {
					sourceID = "456"
				}
				payload := fmt.Sprintf(`{"number":42,"state":"open","head":{"ref":"feature/same-name","sha":"%s","repo":{"id":%s,"name":"app","owner":{"login":%q},"html_url":"https://forge.example/%s/app"}},"base":{"ref":"main","sha":"%s","repo":{"id":123,"name":"app","owner":{"login":"org"},"html_url":"https://forge.example/org/app"}}}`,
					strings.Repeat("a", 40), sourceID, sourceOwner, sourceOwner, strings.Repeat("b", 40))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/pulls/42"):
						_, _ = w.Write([]byte(payload))
					case strings.HasSuffix(r.URL.Path, "/pulls"):
						_, _ = fmt.Fprintf(w, `[%s]`, payload)
					case strings.HasSuffix(r.URL.Path, "/reviews"), strings.HasSuffix(r.URL.Path, "/comments"):
						_, _ = w.Write([]byte(`[]`))
					case strings.HasSuffix(r.URL.Path, "/status"):
						_, _ = w.Write([]byte(`{"state":"success","statuses":[]}`))
					case strings.HasSuffix(r.URL.Path, "/check-runs"):
						_, _ = w.Write([]byte(`{"check_runs":[]}`))
					default:
						t.Errorf("unexpected endpoint %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				var provider Provider
				if kind == ProviderGitHub {
					provider = NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
				} else {
					provider = NewGiteaProvider(server.URL, "token")
				}
				base := RepositoryRef{Provider: kind, Owner: "org", Name: "app"}
				list, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{Repository: base, SkipCheckState: true})
				if err != nil || len(list) != 1 {
					t.Fatalf("list = %+v, %v", list, err)
				}
				poll, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: base, PullID: "42"})
				if err != nil {
					t.Fatal(err)
				}
				if poll.HeadRepository == nil || poll.HeadRepository.ID != sourceID ||
					poll.HeadRepository.Owner != sourceOwner ||
					!reflect.DeepEqual(poll.HeadRepository, list[0].HeadRepository) ||
					!reflect.DeepEqual(poll.BaseRepository, list[0].BaseRepository) ||
					poll.HeadSHA != list[0].HeadSHA {
					t.Fatalf("list and poll source binding differ: %+v / %+v", list[0], poll)
				}
			})
		}
	}
}
