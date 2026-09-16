package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestRepositoryIdentityLosslessProjection(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea, ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			repo := RepositoryRef{
				Provider: kind, Owner: "Org", Project: "Project", Name: "Repository",
				ID: "immutable-id", URL: "https://forge.example:8443/collection/Org/Repository",
			}
			identity := repo.RepositoryIdentity()
			if got := RepositoryRefFromIdentity(identity); got != repo {
				t.Fatalf("round trip = %+v, want %+v", got, repo)
			}
			if identity.CanonicalKey() != repo.CanonicalKey() {
				t.Fatalf("projection changed canonical identity")
			}
		})
	}
}

func TestForgePullRequestSummaryRepositoryFixtures(t *testing.T) {
	const baseJSON = `{"id":123,"name":"app","owner":{"login":"org"},"html_url":"https://forge.example/org/app"}`
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		for _, tc := range []struct {
			name, headJSON, owner, repo, id, host string
		}{
			{"same repository", baseJSON, "org", "app", "123", "forge.example"},
			{"fork", `{"id":456,"name":"app","owner":{"login":"contributor"},"html_url":"https://forge.example/contributor/app"}`, "contributor", "app", "456", "forge.example"},
			{"other service", `{"id":123,"name":"app","owner":{"login":"org"},"html_url":"https://other.example/org/app"}`, "org", "app", "123", "other.example"},
		} {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				payload := fmt.Sprintf(`{"number":42,"state":"open","head":{"ref":"feature/same-name","sha":"%s","repo":%s},"base":{"ref":"main","sha":"%s","repo":%s}}`,
					strings.Repeat("a", 40), tc.headJSON, strings.Repeat("b", 40), baseJSON)
				var summary PullRequestSummary
				if kind == ProviderGitHub {
					var pr githubPullRequestDetail
					if err := json.Unmarshal([]byte(payload), &pr); err != nil {
						t.Fatal(err)
					}
					summary = summarizePullRequest(pr, CheckStatePassing)
				} else {
					var pr giteaPull
					if err := json.Unmarshal([]byte(payload), &pr); err != nil {
						t.Fatal(err)
					}
					summary = summarizeGiteaPull(pr, CheckStatePassing)
				}
				want := RepositoryRef{Provider: kind, Owner: tc.owner, Name: tc.repo, ID: tc.id,
					URL: "https://" + tc.host + "/" + tc.owner + "/" + tc.repo}
				if summary.HeadRepository == nil || *summary.HeadRepository != want {
					t.Fatalf("head repository = %+v, want %+v", summary.HeadRepository, want)
				}
				if summary.BaseRepository == nil || summary.BaseRepository.ID != "123" || summary.HeadSHA != strings.Repeat("a", 40) {
					t.Fatalf("lost snapshot identity: %+v", summary)
				}
				if SameRepository(*summary.HeadRepository, *summary.BaseRepository) != (tc.name == "same repository") {
					t.Fatalf("duplicate branch or repository name conflated source and base")
				}
				data, err := json.Marshal(summary)
				if err != nil {
					t.Fatal(err)
				}
				var restored PullRequestSummary
				if err := json.Unmarshal(data, &restored); err != nil ||
					!reflect.DeepEqual(restored.HeadRepository, summary.HeadRepository) ||
					!reflect.DeepEqual(restored.BaseRepository, summary.BaseRepository) ||
					restored.HeadSHA != summary.HeadSHA || restored.BaseSHA != summary.BaseSHA {
					t.Fatalf("summary persistence lost identity: %+v, %v", restored, err)
				}
			})
		}
	}
}

func TestADOPullRequestRepositoryFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, fork, project, repoID, repoName, repoURL string
	}{
		{"same repository", "", "base-project", "base-id", "app", "https://ado.example/collection/org/base-project/_git/app"},
		{"cross project fork", `,"forkSource":{"name":"refs/heads/feature/same-name","repository":{"id":"fork-id","name":"app","remoteUrl":"https://ado.example/collection/org/fork-project/_git/app","project":{"id":"fork-project-id","name":"fork-project"}}}`, "fork-project", "fork-id", "app", "https://ado.example/collection/org/fork-project/_git/app"},
		{"renamed fork", `,"forkSource":{"repository":{"id":"fork-id","name":"renamed","remoteUrl":"https://ado.example/collection/org/base-project/_git/renamed","project":{"name":"base-project"}}}`, "base-project", "fork-id", "renamed", "https://ado.example/collection/org/base-project/_git/renamed"},
		{"missing fork repository", `,"forkSource":{"name":"refs/heads/feature/same-name"}`, "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"pullRequestId":42,"status":"active","sourceRefName":"refs/heads/feature/same-name","targetRefName":"refs/heads/main","lastMergeSourceCommit":{"commitId":"%s"},"lastMergeTargetCommit":{"commitId":"%s"},"repository":{"id":"base-id","name":"app","remoteUrl":"https://ado.example/collection/org/base-project/_git/app","project":{"id":"base-project-id","name":"base-project"}}%s}`,
				strings.Repeat("a", 40), strings.Repeat("b", 40), tc.fork)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/policy/evaluations"):
					_, _ = w.Write([]byte(`{"value":[]}`))
				case strings.HasSuffix(r.URL.Path, "/pullrequests/42"):
					_, _ = w.Write([]byte(payload))
				case strings.HasSuffix(r.URL.Path, "/pullrequests"):
					_, _ = fmt.Fprintf(w, `{"value":[%s]}`, payload)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}

			}))
			defer server.Close()
			provider := NewADOProvider("org", "base-project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			base := RepositoryRef{Provider: ProviderADO, Owner: "org", Project: "base-project", Name: "app"}
			summaries, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{Repository: base})
			if err != nil || len(summaries) != 1 {
				t.Fatalf("list = %+v, %v", summaries, err)
			}
			poll, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: base, PullID: "42"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(summaries[0].HeadRepository, poll.HeadRepository) ||
				!reflect.DeepEqual(summaries[0].BaseRepository, poll.BaseRepository) {
				t.Fatalf("list/poll repository mismatch: %+v / %+v", summaries[0], poll)
			}
			if tc.repoID == "" {
				if poll.HeadRepository != nil {
					t.Fatalf("missing fork silently substituted base: %+v", poll.HeadRepository)
				}
				return
			}
			want := RepositoryRef{Provider: ProviderADO, Owner: "org", Project: tc.project, Name: tc.repoName, ID: tc.repoID, URL: tc.repoURL}
			if poll.HeadRepository == nil || *poll.HeadRepository != want {
				t.Fatalf("source = %+v, want %+v", poll.HeadRepository, want)
			}
			if SameRepository(*poll.HeadRepository, *poll.BaseRepository) != (tc.name == "same repository") {
				t.Fatal("ADO source identity conflated same-named branches or repositories")
			}
		})
	}
}
