package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workbench"
)

type workbenchStub struct {
	calls           int
	principal       Principal
	gaggle, binding string
	page            workbench.BacklogPageRequest
	item            workbench.BacklogItemRequest
	refusal         error
}

func (s *workbenchStub) record(p Principal, g, b string) error {
	s.calls++
	s.principal = p
	s.gaggle = g
	s.binding = b
	return s.refusal
}
func (s *workbenchStub) Sources(_ context.Context, p Principal, g string) (workbench.SourcePage, error) {
	return workbench.SourcePage{Items: []workbench.SourceView{}, Generation: strings.Repeat("a", 64)}, s.record(p, g, "")
}
func (s *workbenchStub) Page(_ context.Context, p Principal, g, b string, r workbench.BacklogPageRequest) (workbench.BacklogPage, error) {
	s.page = r
	return workbench.BacklogPage{Items: []workbench.BacklogItem{}}, s.record(p, g, b)
}
func (s *workbenchStub) Get(_ context.Context, p Principal, g, b string, r workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
	s.item = r
	return workbench.BacklogItem{}, s.record(p, g, b)
}
func TestWorkbenchReadRoutesBindHumanAndClosedSourceQueries(t *testing.T) {
	service := &workbenchStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleView}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(auth), WithWorkbenchReads(service))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		want int
	}{
		{"", 200}, {"/items/items?limit=25&cursor=opaque", 200}, {"/items/items/42?expectedSourceId=987654", 200},
		{"?credential=automation", 400}, {"/items/items?limit=101", 400}, {"/items/items?limit=0", 400}, {"/items/items?limit=1&limit=2", 400}, {"/items/items?gaggle=foreign", 400}, {"/items/items?cursor=" + strings.Repeat("x", 2049), 400}, {"/items/items/0042", 400}, {"/items/items/42?sourceId=other", 400}, {"/items/items/42?expectedSourceId=" + strings.Repeat("x", 513), 400},
	} {
		before := service.calls
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "http://example.com/api/v1/gaggles/team/workbench/sources"+test.path, nil))
		if out.Code != test.want || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(test.path, out.Code, out.Body)
		}
		if test.want == 200 && (service.calls != before+1 || service.principal.Subject != "alice" || service.gaggle != "team") {
			t.Fatal("verified actor/source lost")
		}
		if test.want != 200 && service.calls != before {
			t.Fatal("malformed request reached service")
		}
	}
	if service.page.Limit != 25 || service.page.Cursor != "opaque" || service.item.ID != "42" || service.item.ExpectedSourceID != "987654" {
		t.Fatal("request projection differs")
	}
}
func TestWorkbenchReadRoutesRequireHumanAndPropagateSourceDenial(t *testing.T) {
	for _, p := range []*Principal{nil, {Subject: "worker", Issuer: PodPrincipalIssuer, Roles: []Role{RoleAdmin}}} {
		service := &workbenchStub{}
		handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: p}), WithWorkbenchReads(service))
		if err != nil {
			t.Fatal(err)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "http://example.com/api/v1/gaggles/team/workbench/sources", nil))
		if out.Code < 400 || service.calls != 0 {
			t.Fatal("nonhuman reached source service", out.Code)
		}
	}
	service := &workbenchStub{refusal: &InterventionError{Status: http.StatusForbidden, Code: "interactive_access_denied", Message: "Source access denied."}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: &Principal{Subject: "alice", Issuer: "https://identity.example", Roles: []Role{RoleView}}}), WithWorkbenchReads(service))
	if err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "http://example.com/api/v1/gaggles/team/workbench/sources/items/items", nil))
	if out.Code != 403 || out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(out.Code, out.Body)
	}
}
