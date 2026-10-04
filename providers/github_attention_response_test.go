package providers

import (
	"net/http"
	"strings"
	"testing"
)

func TestNeedsHumanRejectsAmbiguousGitHubPageEvidence(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"no-content", "", 204}, {"unexpected-success", "[]", 201},
		{"empty", "", 200}, {"null", "null", 200}, {"object", "{}", 200},
		{"trailing-value", "[] {}", 200}, {"trailing-garbage", "[] broken", 200},
		{"null-member", "[null]", 200}, {"scalar-member", "[42]", 200},
		{"oversized", "[]" + strings.Repeat(" ", MaxAttentionResponseBytes), 200},
	} {
		for _, page := range []string{"comments", "blocked_by"} {
			t.Run(test.name+"/"+page, func(t *testing.T) {
				client := NewGitHubProvider("human", WithHTTPClient(&http.Client{Transport: attentionTransport(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/"+page) {
						return attentionResponse(r, test.status, test.body), nil
					}
					if strings.HasSuffix(r.URL.Path, "/comments") {
						return attentionResponse(r, 200, "[]"), nil
					}
					return attentionResponse(r, 200, attentionGitHubItem), nil
				})}))
				result, err := client.InspectNeedsHuman(t.Context(), RepositoryRef{Owner: "acme", Name: "issues"}, "42")
				if err == nil || result.BlockersComplete || (page == "comments" && result.CommentsComplete) {
					t.Fatal("ambiguous evidence certified completion", result, err)
				}
			})
		}
	}
}

func TestNeedsHumanGitHubFirstPageCoverageRequiresAbsentLink(t *testing.T) {
	for _, test := range []struct {
		name  string
		links []string
	}{
		{"absent", nil}, {"blank", []string{""}}, {"malformed", []string{"not a link"}},
		{"unknown", []string{`<https://api.github.com/unknown>; rel="mystery"`}},
		{"next", []string{`<https://api.github.com/next>; rel="next"`}},
		{"multiple", []string{`<https://api.github.com/self>; rel="first"`, `malformed`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewGitHubProvider("human", WithHTTPClient(&http.Client{Transport: attentionTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/blocked_by") {
					response := attentionResponse(r, 200, " \n [] \t ")
					for _, link := range test.links {
						response.Header.Add("Link", link)
					}
					return response, nil
				}
				return attentionResponse(r, 200, attentionGitHubItem), nil
			})}))
			result, err := client.InspectNeedsHuman(t.Context(), RepositoryRef{Owner: "acme", Name: "issues"}, "42")
			if err != nil || result.CommentsComplete != (test.links == nil) || result.BlockersComplete != (test.links == nil) {
				t.Fatal("ambiguous pagination certified completion", result, err)
			}
		})
	}
}
