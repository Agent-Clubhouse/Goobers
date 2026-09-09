package providers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestGitHubSharedClaimUsesNonForcedChildCommit(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const key = "github/acme/repo/issues/42"
			oldSHA, newSHA, treeSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
			now := time.Now().UTC().Truncate(time.Second)
			prior, err := sharedclaim.Encode(key, sharedclaim.Record{Version: 1,
				Owner: sharedclaim.Owner{Instance: "old", Run: "run", Token: "old-token"}, ExpiresAt: now.Add(-time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			owner := sharedclaim.Owner{Instance: "new", Run: "run", Token: "new-token"}
			patches := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Date", now.Format(http.TimeFormat))
				if r.Header.Get("Authorization") != "Bearer token" {
					t.Error("shared claim bypassed provider authentication")
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/git/ref/"+strings.TrimPrefix(sharedClaimRef(key), "refs/"):
					_ = json.NewEncoder(w).Encode(map[string]any{"ref": sharedClaimRef(key), "object": map[string]string{"type": "commit", "sha": oldSHA}})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/git/commits/"+oldSHA:
					_ = json.NewEncoder(w).Encode(map[string]any{"sha": oldSHA, "message": string(prior), "tree": map[string]string{"sha": treeSHA}})
				case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/repo/git/commits":
					var body struct {
						Message string
						Tree    string
						Parents []string
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.Tree != treeSHA || len(body.Parents) != 1 || body.Parents[0] != oldSHA {
						t.Error("new coordination commit is not a child of the observed revision")
					}
					got, err := sharedclaim.Decode(key, []byte(body.Message))
					if err != nil || got.Owner != owner || !got.ExpiresAt.Equal(now.Add(time.Minute)) {
						t.Errorf("new coordination lease: %+v %v", got, err)
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"sha": newSHA})
				case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/repo/git/refs/"+strings.TrimPrefix(sharedClaimRef(key), "refs/"):
					patches++
					var body struct {
						SHA   string
						Force *bool
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.SHA != newSHA || body.Force == nil || *body.Force {
						t.Error("shared claim used an unconditional ref update")
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{}`))
				default:
					t.Errorf("unexpected shared claim request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL; p.maxRetries = 0 })
			store := GitHubSharedClaimStore{Provider: provider, Repository: RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "repo"}}
			err = sharedclaim.Acquire(t.Context(), store, key, owner, time.Minute)
			if status == http.StatusConflict {
				if !errors.Is(err, sharedclaim.ErrConflict) {
					t.Fatalf("contention admitted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if patches != 1 {
				t.Fatalf("CAS attempts = %d", patches)
			}
		})
	}
}
