package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workbench"
)

func (s *workbenchStub) Documents(_ context.Context, p Principal, g, b string, r workbench.DocumentPageRequest) (workbench.DocumentPage, error) {
	s.page = workbench.BacklogPageRequest(r)
	return workbench.DocumentPage{Files: []workbench.DocumentFileRead{}}, s.record(p, g, b)
}
func TestWorkbenchDocumentsRequireHumanAndClosedConfiguredPage(t *testing.T) {
	service := &workbenchStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleView}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(auth), WithWorkbenchDocuments(service))
	if err != nil {
		t.Fatal(err)
	}
	path := "http://example.com/api/v1/gaggles/team/workbench/sources/strategy/documents"
	for _, tc := range []struct {
		query  string
		status int
	}{{"", 200}, {"?limit=2&cursor=opaque", 200}, {"?limit=9", 400}, {"?limit=0", 400}, {"?limit=1&limit=2", 400}, {"?path=secret.md", 400}, {"?commit=old", 400}, {"?credential=automation", 400}, {"?cursor=" + strings.Repeat("a", 2049), 400}} {
		before := service.calls
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+tc.query, nil))
		if response.Code != tc.status || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(tc.query, response.Code, response.Body)
		}
		if tc.status == 200 && (service.calls != before+1 || service.gaggle != "team" || service.binding != "strategy" || service.principal.Subject != "alice") {
			t.Fatal("lost source or human scope")
		}
		if tc.status != 200 && service.calls != before {
			t.Fatal("invalid query reached source")
		}
	}
	if service.page.Limit != 2 || service.page.Cursor != "opaque" {
		t.Fatal("pagination changed")
	}
	for _, principal := range []*Principal{nil, {Subject: "worker", Issuer: PodPrincipalIssuer, Roles: []Role{RoleAdmin}}} {
		auth.principal = principal
		before := service.calls
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code < 400 || service.calls != before {
			t.Fatal("worker read source documents")
		}
	}
}
