package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestVerdictObservationPollProvenance(t *testing.T) {
	for _, forge := range []string{"github", "gitea"} {
		for _, tc := range []struct {
			name, body    string
			status        int
			commentsError bool
			want          VerdictReadStatus
			phase         string
		}{
			{name: "explicit-empty", body: `{"statuses":[],"total_count":0}`, want: VerdictReadComplete},
			{name: "missing", body: `{}`, want: VerdictReadIncomplete},
			{name: "null-response", body: `null`, want: VerdictReadIncomplete},
			{name: "null-list", body: `{"statuses":null,"total_count":0}`, want: VerdictReadIncomplete},
			{name: "bad-entry", body: `{"statuses":[{}],"total_count":1}`, want: VerdictReadIncomplete},
			{name: "bad-state", body: `{"statuses":[{"context":"build","state":"mystery","status":"mystery"}],"total_count":1}`, want: VerdictReadIncomplete},
			{name: "decode", body: `{"statuses":`, phase: "checks"},
			{name: "authentication", status: http.StatusUnauthorized, phase: "checks"},
			{name: "comment-error", body: `{"statuses":[],"total_count":0}`, commentsError: true, phase: "comments"},
		} {
			t.Run(forge+"/"+tc.name, func(t *testing.T) {
				var statusReads atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
					}
					switch {
					case strings.HasSuffix(r.URL.Path, "/pulls/9"):
						_, _ = fmt.Fprint(w, `{"number":9,"state":"open","head":{"sha":"head"},"base":{"sha":"base"}}`)
					case strings.HasSuffix(r.URL.Path, "/status"):
						statusReads.Add(1)
						if tc.status != 0 {
							w.WriteHeader(tc.status)
						}
						_, _ = fmt.Fprint(w, tc.body)
					case strings.HasSuffix(r.URL.Path, "/check-runs"):
						_, _ = fmt.Fprint(w, `{"check_runs":[]}`)
					case strings.HasSuffix(r.URL.Path, "/comments") && tc.commentsError:
						w.WriteHeader(http.StatusUnauthorized)
					default:
						_, _ = fmt.Fprint(w, `[]`)
					}
				}))
				defer server.Close()
				var p Provider
				if forge == "github" {
					p = NewGitHubProvider("fixture", func(p *GitHubProvider) { p.BaseURL = server.URL })
				} else {
					p = NewGiteaProvider(server.URL, "fixture")
				}
				poll, err := p.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9"})
				if statusReads.Load() != 1 {
					t.Fatalf("native status reads = %d, want exactly the existing poll read", statusReads.Load())
				}
				if (err != nil) != (tc.phase != "") || poll.PollErrorPhase != tc.phase || poll.VerdictReadStatus != tc.want {
					t.Fatalf("poll=%+v err=%v want read=%q phase=%q", poll, err, tc.want, tc.phase)
				}
				if tc.phase == "" && tc.name != "bad-entry" && tc.name != "bad-state" && poll.CheckState != CheckStatePassing {
					t.Fatalf("shadow changed existing empty-status merge behavior: %+v", poll)
				}
			})
		}
	}
}

func TestVerdictObservationGiteaPartialPageIsUnknown(t *testing.T) {
	for _, body := range []string{`{"statuses":[]}`, `{"statuses":[],"total_count":2}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
		p := NewGiteaProvider(server.URL, "fixture")
		state, _, _, read, err := p.combinedCheckStateAndVerdict(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "head")
		server.Close()
		if err != nil || state != CheckStatePassing || read != VerdictReadIncomplete {
			t.Fatalf("state=%s read=%s err=%v", state, read, err)
		}
	}
}
