package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workbench"
)

type suggestionStub struct {
	calls  int
	actor  Principal
	gaggle string
	input  workbench.SuggestionDecisionRequest
}

func (s *suggestionStub) record(p Principal, g string) { s.calls++; s.actor = p; s.gaggle = g }
func (s *suggestionStub) Artifacts(_ context.Context, p Principal, g, run string, after uint64) (workbench.SuggestionInventory, error) {
	s.record(p, g)
	return workbench.SuggestionInventory{RunID: run, NextSequence: after}, nil
}
func (s *suggestionStub) Load(_ context.Context, p Principal, g string, input workbench.SuggestionSelection) (workbench.SuggestionBatch, error) {
	s.record(p, g)
	return workbench.SuggestionBatch{Selection: input}, nil
}
func (s *suggestionStub) Preview(_ context.Context, p Principal, g string, input workbench.SuggestionPreviewRequest) (workbench.SuggestionPreview, error) {
	s.record(p, g)
	return workbench.SuggestionPreview{}, nil
}
func (s *suggestionStub) Decide(_ context.Context, p Principal, g string, input workbench.SuggestionDecisionRequest) (workbench.SuggestionReview, error) {
	s.record(p, g)
	s.input = input
	return workbench.SuggestionReview{State: "linked"}, nil
}
func (s *suggestionStub) Review(_ context.Context, p Principal, g, id string) (workbench.SuggestionReview, error) {
	s.record(p, g)
	return workbench.SuggestionReview{ID: id}, nil
}
func suggestionHandler(t *testing.T, s *suggestionStub, p *Principal) http.Handler {
	t.Helper()
	h, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: p}), WithWorkbenchSuggestions(s))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const suggestionBase = "http://example.com/api/v1/gaggles/team/workbench/suggestions"

func suggestionBody() string {
	return `{"selection":{"runId":"` + strings.Repeat("a", 32) + `","sequence":3},"key":"` + strings.Repeat("b", 64) + `","decision":"accept","expectedOwner":{"commit":"` + strings.Repeat("c", 40) + `","blobId":"` + strings.Repeat("d", 40) + `","contentDigest":"` + strings.Repeat("e", 64) + `"},"expectedOperationDigest":"` + strings.Repeat("f", 64) + `"}`
}
func TestWorkbenchSuggestionClosedAuthorityAndTransport(t *testing.T) {
	s := &suggestionStub{}
	h := suggestionHandler(t, s, &Principal{Issuer: "issuer", Subject: "human", Roles: []Role{RoleOperate}})
	run := strings.Repeat("a", 32)
	for _, route := range []string{"/artifacts/" + run, "/artifacts/" + run + "?after=3", "/artifacts/" + run + "/3", "/reviews/workbench-" + run} {
		out := nativeWriteRequest(h, "GET", suggestionBase+route, "", "")
		if out.Code != 200 || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(route, out.Code, out.Body)
		}
	}
	if out := nativeWriteRequest(h, "POST", suggestionBase+"/decisions", suggestionBody(), ""); out.Code != 200 || s.actor.Subject != "human" || s.gaggle != "team" || s.input.Decision != "accept" {
		t.Fatal(out.Code, out.Body, s)
	}
	before := s.calls
	for _, body := range []string{"null", suggestionBody() + "{}", strings.Replace(suggestionBody(), `"selection":{`, `"selection":{"actor":"other",`, 1), strings.Replace(suggestionBody(), `"key":`, `"Key":`, 1), strings.Replace(suggestionBody(), `"expectedOwner":{`, `"expectedOwner":{"credentialRef":"automation",`, 1), strings.Replace(suggestionBody(), `"sequence":3`, `"sequence":3,"sequence":4`, 1), strings.Replace(suggestionBody(), `"decision":"accept"`, `"decision":"reject"`, 1), strings.Repeat(" ", 16384) + suggestionBody()} {
		if out := nativeWriteRequest(h, "POST", suggestionBase+"/decisions", body, ""); out.Code != 400 {
			t.Fatal(out.Code, out.Body)
		}
	}
	for _, route := range []string{"/artifacts/" + run + "?after=1&after=2", "/artifacts/" + run + "?credential=automation", "/artifacts/" + run + "/0", "/artifacts/" + run + "/3?actor=other"} {
		if out := nativeWriteRequest(h, "GET", suggestionBase+route, "", ""); out.Code != 400 {
			t.Fatal(out.Code, out.Body)
		}
	}
	if s.calls != before {
		t.Fatal("malformed selection reached service")
	}
}
func TestWorkbenchSuggestionHumanOnlyDecisions(t *testing.T) {
	for _, p := range []*Principal{nil, {Issuer: PodPrincipalIssuer, Subject: "pod", Roles: []Role{RoleAdmin}}, {Issuer: "issuer", Subject: "human", Roles: []Role{RoleView}}, {Issuer: "issuer", Subject: "human", Roles: []Role{RoleAdmin}, GeneratedChild: true}} {
		s := &suggestionStub{}
		h := suggestionHandler(t, s, p)
		out := nativeWriteRequest(h, "POST", suggestionBase+"/decisions", suggestionBody(), "")
		if out.Code < 400 || s.calls != 0 {
			t.Fatal(out.Code, s.calls)
		}
	}
}
