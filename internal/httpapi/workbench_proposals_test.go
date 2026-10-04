package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workbench"
)

type proposalStub struct {
	calls                               int
	operation, gaggle, binding, key, id string
	actor                               Principal
	request                             workbench.MetadataChangeRequest
	preview                             workbench.MetadataPreview
	refusal                             error
}

func (s *proposalStub) record(operation string, p Principal, g, b string) error {
	s.calls++
	s.operation, s.actor, s.gaggle, s.binding = operation, p, g, b
	return s.refusal
}
func (s *proposalStub) Preview(_ context.Context, p Principal, g, b string, r workbench.MetadataChangeRequest) (workbench.MetadataPreview, error) {
	s.request = r
	return s.preview, s.record("preview", p, g, b)
}
func (s *proposalStub) Submit(_ context.Context, p Principal, g, b, key string, r workbench.MetadataChangeRequest) (workbench.MetadataProposalCommand, error) {
	s.key, s.request = key, r
	return workbench.MetadataProposalCommand{State: "unknown"}, s.record("submit", p, g, b)
}
func (s *proposalStub) Continue(_ context.Context, p Principal, g, b, id string) (workbench.MetadataProposalCommand, error) {
	s.id = id
	return workbench.MetadataProposalCommand{State: "prepared"}, s.record("continue", p, g, b)
}
func (s *proposalStub) Command(_ context.Context, p Principal, g, b, id string) (workbench.MetadataProposalCommand, error) {
	s.id = id
	return workbench.MetadataProposalCommand{State: "unknown"}, s.record("command", p, g, b)
}
func (s *proposalStub) Check(_ context.Context, p Principal, g, b, id string) (workbench.MetadataProposalCommand, error) {
	s.id = id
	return workbench.MetadataProposalCommand{State: "observed"}, s.record("check", p, g, b)
}

const proposalBase = "http://example.com/api/v1/gaggles/team/workbench/sources/strategy"

func proposalBody() string {
	return `{"path":"plan.md","expected":{"commit":"` + strings.Repeat("a", 40) + `","blobId":"` + strings.Repeat("b", 40) + `","contentDigest":"` + strings.Repeat("c", 64) + `"},"field":"description","value":"Body"}`
}
func proposalRouteHandler(t *testing.T, s *proposalStub, p *Principal) http.Handler {
	t.Helper()
	h, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: p}), WithWorkbenchProposals(s))
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func TestWorkbenchProposalRoutesBindOnlyTransportAuthority(t *testing.T) {
	s := &proposalStub{}
	h := proposalRouteHandler(t, s, &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleOperate}})
	command := "workbench-" + strings.Repeat("a", 32)
	for _, tc := range []struct{ path, method, body, key, operation string }{{"/proposal-preview", "POST", proposalBody(), "", "preview"}, {"/proposals", "POST", proposalBody(), "human-key", "submit"}, {"/proposals/" + command, "GET", "", "", "command"}, {"/proposals/" + command + "/check", "POST", "{}", "", "check"}, {"/proposals/" + command + "/continue", "POST", "{}", "", "continue"}} {
		response := nativeWriteRequest(h, tc.method, proposalBase+tc.path, tc.body, tc.key)
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || s.operation != tc.operation || s.actor.Subject != "alice" || s.gaggle != "team" || s.binding != "strategy" {
			t.Fatal(tc.operation, response.Code, response.Body, s)
		}
	}
	if s.key != "human-key" || s.request.Path != "plan.md" || s.id != command {
		t.Fatal(s)
	}
}
func TestWorkbenchProposalClosedNestedInputsAndRevisionBounds(t *testing.T) {
	s := &proposalStub{}
	h := proposalRouteHandler(t, s, &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleOperate}})
	original := proposalBody()
	for _, body := range []string{"null", "[]", "{}", original + "{}", strings.Replace(original, `"path":`, `"Path":`, 1), strings.Replace(original, `"path":"plan.md"`, `"path":"plan.md","path":"other.md"`, 1), strings.Replace(original, `"commit":`, `"Commit":`, 1), strings.Replace(original, `"commit":"`+strings.Repeat("a", 40)+`"`, `"commit":"`+strings.Repeat("a", 40)+`","commit":"`+strings.Repeat("a", 40)+`"`, 1), strings.Replace(original, `"expected":{`, `"expected":{"credentialRef":"automation",`, 1), strings.Replace(original, `"value":"Body"`, `"value":null`, 1), strings.Replace(original, `"value":"Body"`, `"value":"Body","actor":"other"`, 1), strings.Replace(original, `"value":"Body"`, `"value":"Body","relationship":{}`, 1), strings.Replace(original, "plan.md", "../plan.md", 1), strings.Replace(original, strings.Repeat("a", 40), "historical-ref", 1), strings.Repeat(" ", maxMetadataRequestBytes) + original} {
		out := nativeWriteRequest(h, "POST", proposalBase+"/proposals", body, "one")
		if out.Code != 400 {
			t.Fatalf("accepted malformed metadata: status%d %.160s", out.Code, body)
		}
	}
	if s.calls != 0 {
		t.Fatal("malformed body reached service")
	}
	// Valid relationship input is closed recursively, including reference scope.
	from := workbench.NodeRef{GaggleID: "team", SourceBindingID: "items", Kind: "work-item", SourceID: "9876"}
	to := workbench.NodeRef{GaggleID: "team", SourceBindingID: "strategy", Kind: "objective-document", SourceID: "obj-00000000-0000-0000-0000-000000000001"}
	request := workbench.MetadataChangeRequest{Path: "links.yaml", Expected: workbench.MetadataRevision{Commit: strings.Repeat("a", 40), BlobID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64)}, Relationship: &workbench.MetadataRelationshipEdit{Action: "add", Edge: workbench.Edge{EdgeID: "edge-00000000-0000-0000-0000-000000000001", Kind: "references", From: from, To: to}}}
	raw, _ := json.Marshal(request)
	if out := nativeWriteRequest(h, "POST", proposalBase+"/proposal-preview", string(raw), ""); out.Code != 200 {
		t.Fatal(out.Code, out.Body)
	}
	before := s.calls
	for _, body := range []string{strings.Replace(string(raw), `"sourceId":"9876"`, `"sourceId":"9876","sourceId":"other"`, 1), strings.Replace(string(raw), `"from":{`, `"from":{"credential":"secret",`, 1), strings.Replace(string(raw), `"gaggleId":"team"`, `"gaggleId":"other"`, 1), strings.Replace(string(raw), `"kind":"references"`, `"kind":"parent-of"`, 1)} {
		if out := nativeWriteRequest(h, "POST", proposalBase+"/proposal-preview", body, ""); out.Code != 400 {
			t.Fatal("nested injection accepted", out.Code, out.Body)
		}
	}
	if s.calls != before {
		t.Fatal("invalid reference reached service")
	}
}
func TestWorkbenchProposalHumanTransportAndFiniteResponses(t *testing.T) {
	for _, p := range []*Principal{nil, {Issuer: PodPrincipalIssuer, Subject: "pod", Roles: []Role{RoleAdmin}}, {Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleView}}, {Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleAdmin}, GeneratedChild: true}} {
		s := &proposalStub{}
		h := proposalRouteHandler(t, s, p)
		if out := nativeWriteRequest(h, "POST", proposalBase+"/proposals", proposalBody(), "one"); out.Code < 400 || s.calls != 0 {
			t.Fatal(out.Code, s.calls)
		}
	}
	s := &proposalStub{}
	h := proposalRouteHandler(t, s, &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleOperate}})
	command := "workbench-" + strings.Repeat("a", 32)
	for _, body := range []string{"", "null", `{"phase":"pull-request"}`, `{"actor":"other"}`, `{} {}`, strings.Repeat(" ", 513) + `{}`} {
		if out := nativeWriteRequest(h, "POST", proposalBase+"/proposals/"+command+"/continue", body, ""); out.Code != 400 {
			t.Fatal(out.Code, out.Body)
		}
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Header.Add(HeaderIdempotencyKey, "two") }, func(r *http.Request) { r.Header.Set("Origin", "https://foreign.example") }, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, func(r *http.Request) { r.URL.RawQuery = "branch=main" }} {
		r := httptest.NewRequest("POST", proposalBase+"/proposals", strings.NewReader(proposalBody()))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(HeaderIdempotencyKey, "one")
		change(r)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		if out.Code != 400 {
			t.Fatal(out.Code, out.Body)
		}
	}
	if s.calls != 0 {
		t.Fatal("invalid command reached service")
	}
	s.preview.Before = strings.Repeat("x", workbench.MaxMetadataPreviewBytes)
	if out := nativeWriteRequest(h, "POST", proposalBase+"/proposal-preview", proposalBody(), ""); out.Code != 502 {
		t.Fatal("oversized response", out.Code)
	}
}
