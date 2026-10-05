package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type childMonitorStub struct {
	calls       int
	principal   Principal
	run, cursor string
}

func (s *childMonitorStub) ListChildWorkflows(_ context.Context, p Principal, run, after string) (apicontract.ChildWorkflowPage, error) {
	s.calls++
	s.principal, s.run, s.cursor = p, run, after
	return apicontract.ChildWorkflowPage{RunID: run, Gaggle: "own", Children: []apicontract.ChildWorkflowSummary{}}, nil
}

func TestChildMonitorUsesHumanIdentityAndBoundedCursor(t *testing.T) {
	service := &childMonitorStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleView}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(auth), WithChildWorkflowMonitor(service))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"", 200}, {"?after=child-next", 200}, {"?gaggle=other", 400}, {"?after=a&after=b", 400}, {"?after=" + strings.Repeat("a", 129), 400},
	} {
		before := service.calls
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/api/v1/runs/parent/children"+tc.query, nil))
		if out.Code != tc.status || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(out.Code, out.Body)
		}
		if tc.status != 200 && service.calls != before {
			t.Fatal("invalid query reached queue service")
		}
	}
	if service.principal.Subject != "alice" || service.run != "parent" || service.cursor != "child-next" {
		t.Fatal(service)
	}
	before := service.calls
	for _, issuer := range []string{PodPrincipalIssuer, ChildWorkflowPrincipalIssuer, WorkerPrincipalIssuer} {
		auth.principal.Issuer = issuer
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/api/v1/runs/parent/children", nil))
		if out.Code == 200 || service.calls != before {
			t.Fatal("machine principal reached human child monitor", issuer)
		}
	}
}
