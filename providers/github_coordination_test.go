package providers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCoordinationReleaseRequiresPublishedExactTagAndAncestry(t *testing.T) {
	sha, merged := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, mode := range []string{"valid", "draft", "wrong tag", "diverged", "provider failure"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet {
					t.Fatal("release observer attempted mutation")
				}
				switch {
				case strings.Contains(r.URL.Path, "/releases/tags/"):
					if mode == "provider failure" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					tag := "v2.0.0"
					if mode == "wrong tag" {
						tag = "v1.0.0"
					}
					_, _ = fmt.Fprintf(w, `{"tag_name":%q,"draft":%t,"published_at":"2026-09-15T12:00:00Z"}`, tag, mode == "draft")
				case strings.Contains(r.URL.Path, "/commits/"):
					if !strings.HasSuffix(r.URL.Path, "/refs/tags/v2.0.0") {
						t.Errorf("unqualified tag lookup: %s", r.URL.Path)
					}
					_, _ = fmt.Fprintf(w, `{"sha":%q}`, sha)
				case strings.Contains(r.URL.Path, "/compare/"):
					status := "ahead"
					if mode == "diverged" {
						status = "diverged"
					}
					_, _ = fmt.Fprintf(w, `{"status":%q}`, status)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
				}
			}))
			defer server.Close()
			provider := NewGitHubProvider("test-token")
			provider.BaseURL = server.URL
			out, err := provider.GetCoordinationRelease(t.Context(), RepositoryRef{Owner: "acme", Name: "core"}, "v2.0.0", merged)
			if mode == "provider failure" {
				if err == nil {
					t.Fatal("provider failure swallowed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "valid" && (!out.Published || !out.IncludesCommit || out.SHA != sha || calls != 3) {
				t.Fatalf("%+v calls=%d", out, calls)
			}
			if mode == "diverged" && out.IncludesCommit {
				t.Fatal("unrelated release accepted")
			}
			if (mode == "draft" || mode == "wrong tag") && calls != 1 {
				t.Fatal("invalid release treated as published")
			}
		})
	}
}
