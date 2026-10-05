package providers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type attentionTransport func(*http.Request) (*http.Response, error)

func (f attentionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func attentionResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Request: r, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

const attentionGitHubItem = `{"id":99,"number":42,"title":"Question","state":"open","html_url":"https://github.com/acme/issues/issues/42","updated_at":"2026-10-04T12:00:01Z","labels":[{"name":"goobers:needs-human"},{"name":"goobers:ready"},{"name":"ordinary"}]}`
const attentionADOItem = `{"id":42,"rev":7,"url":"https://dev.azure.com/acme/_apis/wit/workItems/42","fields":{"System.TeamProject":"issues","System.Title":"Question","System.WorkItemType":"Bug","System.State":"Active","System.Tags":"ordinary; Goobers:needs-human; goobers:claimed"},"relations":[]}`

func TestNeedsHumanClearIsOneNarrowNonRetryingEffect(t *testing.T) {
	for _, provider := range []ProviderKind{ProviderGitHub, ProviderADO} {
		for _, lost := range []bool{false, true} {
			t.Run(string(provider)+map[bool]string{true: "/lost", false: "/ack"}[lost], func(t *testing.T) {
				mutations := 0
				transport := attentionTransport(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/states") {
						return attentionResponse(r, 200, `{"value":[{"name":"Active","category":"InProgress"}]}`), nil
					}
					item := attentionGitHubItem
					if provider == ProviderADO {
						item = attentionADOItem
					}
					if r.Method == http.MethodGet {
						return attentionResponse(r, 200, item), nil
					}
					mutations++
					if provider == ProviderGitHub {
						if r.Method != http.MethodDelete || r.URL.Path != "/repos/acme/issues/issues/42/labels/goobers:needs-human" {
							t.Fatal("non-narrow effect", r.Method, r.URL)
						}
					} else {
						var patch []adoPatchOperation
						if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
							t.Fatal(err)
						}
						if r.Method != http.MethodPatch || len(patch) != 2 || patch[0].Op != "test" || patch[0].Path != "/rev" || patch[0].Value != float64(7) || patch[1].Path != "/fields/System.Tags" || patch[1].Value != "ordinary; goobers:claimed" {
							t.Fatal("non-narrow ADO effect", patch)
						}
					}
					if lost {
						return attentionResponse(r, 503, `{"message":"uncertain"}`), nil
					}
					return attentionResponse(r, 200, item), nil
				})
				request := NeedsHumanClearRequest{Repository: RepositoryRef{Provider: provider, Owner: "acme", Project: "issues", Name: "issues"}, ID: "42", StableID: "99", ExpectedRevision: "2026-10-04T12:00:01Z"}
				var client NeedsHumanClearer
				if provider == ProviderGitHub {
					client = NewGitHubProvider("human", WithHTTPClient(&http.Client{Transport: transport}))
				} else {
					request.StableID = "42"
					request.ExpectedRevision = "7"
					client = NewADOProvider("acme", "issues", "human", func(p *ADOProvider) { p.Client = &http.Client{Transport: transport} })
				}
				result, err := client.ClearNeedsHuman(t.Context(), request)
				if mutations != 1 || !result.MutationAttempted || result.Acknowledged == lost || (err != nil) != lost {
					t.Fatal(result, err, mutations)
				}
				request.ExpectedRevision = "stale"
				result, err = client.ClearNeedsHuman(t.Context(), request)
				if err == nil || result.MutationAttempted || mutations != 1 {
					t.Fatal("stale mutation", result, err)
				}
				request.ID = "../outside"
				if result, err = client.ClearNeedsHuman(t.Context(), request); err == nil || result.MutationAttempted || mutations != 1 {
					t.Fatal("unsafe locator", result, err)
				}
			})
		}
	}
}
func TestNeedsHumanGitHubInspectionBoundsAndForeignBlockers(t *testing.T) {
	for _, mode := range []string{"complete", "comments-next", "blockers-next", "foreign", "oversized-comment"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := NewGitHubProvider("human", WithHTTPClient(&http.Client{Transport: attentionTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				response := attentionResponse(r, 200, attentionGitHubItem)
				switch {
				case strings.HasSuffix(r.URL.Path, "/comments"):
					body := `[{"id":1,"body":"A specific question","user":{"login":"person","type":"User"}}]`
					if mode == "oversized-comment" {
						body = `[{"id":1,"body":"` + strings.Repeat("x", (64<<10)+1) + `"}]`
					}
					response = attentionResponse(r, 200, body)
					if mode == "comments-next" {
						response.Header.Set("Link", `<https://github.com/next>; rel="next"`)
					}
					if r.URL.Query().Get("per_page") != "100" {
						t.Fatal("comments unbounded")
					}
				case strings.HasSuffix(r.URL.Path, "/blocked_by"):
					body := `[{"id":101,"number":1,"title":"Blocker","state":"closed","html_url":"https://github.com/acme/issues/issues/1"}]`
					if mode == "foreign" {
						body = strings.ReplaceAll(body, "/acme/issues/", "/someone/private/")
					}
					response = attentionResponse(r, 200, body)
					if mode == "blockers-next" {
						response.Header.Set("Link", `<https://github.com/next>; rel="next"`)
					}
					if r.URL.Query().Get("per_page") != "64" {
						t.Fatal("blockers unbounded")
					}
				}
				return response, nil
			})}))
			result, err := client.InspectNeedsHuman(t.Context(), RepositoryRef{Owner: "acme", Name: "issues"}, "42")
			if err != nil || calls != 3 {
				t.Fatal(result, err, calls)
			}
			if result.CommentsComplete != (mode != "comments-next" && mode != "oversized-comment") || result.BlockersComplete != (mode != "blockers-next" && mode != "foreign") {
				t.Fatal("coverage", result)
			}
			if mode == "foreign" && (!result.Blockers[0].Open || result.Blockers[0].Verified) {
				t.Fatal("foreign dependency cleared")
			}
		})
	}
}
func TestNeedsHumanInspectionAndClearRefuseMissingMarker(t *testing.T) {
	client := NewGitHubProvider("human", WithHTTPClient(&http.Client{Transport: attentionTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("markerless effect")
		}
		return attentionResponse(r, 200, strings.ReplaceAll(attentionGitHubItem, "goobers:needs-human", "ordinary-label")), nil
	})}))
	result, err := client.ClearNeedsHuman(t.Context(), NeedsHumanClearRequest{Repository: RepositoryRef{Owner: "acme", Name: "issues"}, ID: "42", StableID: "99", ExpectedRevision: "2026-10-04T12:00:01Z"})
	if !errors.Is(err, ErrAttentionChanged) || result.MutationAttempted {
		t.Fatal(result, err)
	}
}

func TestNeedsHumanADOInspectionKeepsUnverifiedDependenciesBlocked(t *testing.T) {
	for _, mode := range []string{"complete", "comments-next", "foreign", "missing", "category-failed"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			transport := attentionTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				switch {
				case strings.HasSuffix(r.URL.Path, "/comments"):
					if r.URL.Query().Get("$top") != "100" {
						t.Fatal("comments unbounded")
					}
					response := attentionResponse(r, 200, `{"comments":[{"id":1,"text":"A question","createdBy":{"displayName":"Person"}}]}`)
					if mode == "comments-next" {
						response.Header.Set("x-ms-continuationtoken", "another-page")
					}
					return response, nil
				case strings.HasSuffix(strings.ToLower(r.URL.Path), "/workitemsbatch"):
					body := `{"value":[{"id":1,"rev":2,"fields":{"System.TeamProject":"issues","System.WorkItemType":"Task","System.State":"Closed"}}]}`
					if mode == "foreign" {
						body = strings.ReplaceAll(body, `"System.TeamProject":"issues"`, `"System.TeamProject":"private"`)
					}
					if mode == "missing" {
						body = `{"value":[]}`
					}
					return attentionResponse(r, 200, body), nil
				case strings.HasSuffix(r.URL.Path, "/states"):
					if mode == "category-failed" && strings.Contains(r.URL.Path, "/Task/") {
						return attentionResponse(r, 403, `{"message":"denied"}`), nil
					}
					return attentionResponse(r, 200, `{"value":[{"name":"Active","category":"InProgress"},{"name":"Closed","category":"Completed"}]}`), nil
				default:
					body := strings.Replace(attentionADOItem, `"relations":[]`, `"relations":[{"rel":"System.LinkTypes.Dependency-Reverse","url":"https://dev.azure.com/acme/_apis/wit/workItems/1"}]`, 1)
					return attentionResponse(r, 200, body), nil
				}
			})
			client := NewADOProvider("acme", "issues", "human", func(p *ADOProvider) { p.Client = &http.Client{Transport: transport} })
			result, err := client.InspectNeedsHuman(t.Context(), RepositoryRef{Owner: "acme", Project: "issues"}, "42")
			if err != nil || calls > 6 || len(result.Blockers) != 1 {
				t.Fatal(result, err, calls)
			}
			complete := mode == "complete" || mode == "comments-next"
			if result.BlockersComplete != complete || result.CommentsComplete != (mode != "comments-next") {
				t.Fatal("coverage", result)
			}
			if result.Blockers[0].Verified != complete || result.Blockers[0].Open == complete {
				t.Fatal("unverified dependency released", result.Blockers)
			}
		})
	}
}
