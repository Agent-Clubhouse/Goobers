package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/sessioning"
)

type prSelectionStub struct{ calls int }

func (s *prSelectionStub) Inspect(_ context.Context, p Principal, gaggle, source, id string) (sessioning.PRRepairInspection, error) {
	s.calls++
	if p.Subject != "alice" || gaggle != "team" || source != "code" || id != "42" {
		return sessioning.PRRepairInspection{}, sessionBadRequest("Unexpected source")
	}
	return sessioning.PRRepairInspection{}, nil
}

func TestHumanPRSelectionRouteHasClosedReadOnlyScope(t *testing.T) {
	s := &prSelectionStub{}
	p := &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleView}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: p}), WithPRSelection(s))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		suffix string
		status int
	}{{"42", 200}, {"0042", 400}, {"42?credential=automation", 400}, {"42?actor=other", 400}} {
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/api/v1/gaggles/team/workbench/sources/code/pull-requests/"+test.suffix, nil))
		if out.Code != test.status || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(test.suffix, out.Code, out.Body)
		}
	}
	if s.calls != 1 {
		t.Fatal("ambiguous query reached service", s.calls)
	}
	p.GeneratedChild = true
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/api/v1/gaggles/team/workbench/sources/code/pull-requests/42", nil))
	if out.Code != 403 || s.calls != 1 {
		t.Fatal("machine selection reached service", out.Code)
	}
}
