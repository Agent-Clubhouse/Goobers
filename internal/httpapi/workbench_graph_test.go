package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workbenchgraph"
)

type graphServiceStub struct {
	calls     int
	principal Principal
	gaggle    string
	err       error
}

func (s *graphServiceStub) Graph(_ context.Context, p Principal, gaggle string) (workbenchgraph.Graph, error) {
	s.calls++
	s.principal = p
	s.gaggle = gaggle
	return workbenchgraph.Graph{Generation: strings.Repeat("a", 64), GaggleID: gaggle, Nodes: []workbenchgraph.Node{}, Edges: []workbenchgraph.Edge{}, Sources: []workbenchgraph.Coverage{}, Documents: []workbenchgraph.Document{}, Aliases: []workbenchgraph.Alias{}, Conflicts: []workbenchgraph.Conflict{}, Partial: true}, s.err
}
func TestWorkbenchGraphRouteHasNoClientSourceInput(t *testing.T) {
	service := &graphServiceStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleView}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(auth), WithWorkbenchGraph(service))
	if err != nil {
		t.Fatal(err)
	}
	base := "http://example.com/api/v1/gaggles/team/workbench/graph"
	for _, tc := range []struct {
		suffix, body string
		status       int
	}{{"", "", 200}, {"?", "", 400}, {"?source=items", "", 400}, {"?pages=caller", "", 400}, {"", `{"nodes":[],"gaggle":"other"}`, 400}} {
		before := service.calls
		r := httptest.NewRequest(http.MethodGet, base+tc.suffix, strings.NewReader(tc.body))
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		if out.Code != tc.status || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(out.Code, out.Body)
		}
		if tc.status != 200 && service.calls != before {
			t.Fatal("client source input reached service")
		}
	}
	if service.calls != 1 || service.gaggle != "team" || service.principal.Subject != "alice" {
		t.Fatal("verified context lost")
	}
	route, ok := apicontract.V1Route(apicontract.RouteWorkbenchGraph)
	if !ok || route.Budget != apicontract.BoundedBudget {
		t.Fatal("graph route widened cost contract")
	}
}
func TestWorkbenchGraphRequiresHumanAndSanitizesFailure(t *testing.T) {
	for _, p := range []*Principal{nil, {Issuer: PodPrincipalIssuer, Subject: "worker", Roles: []Role{RoleAdmin}}} {
		service := &graphServiceStub{}
		handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: p}), WithWorkbenchGraph(service))
		if err != nil {
			t.Fatal(err)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "http://example.com/api/v1/gaggles/team/workbench/graph", nil))
		if out.Code < 400 || service.calls != 0 {
			t.Fatal("machine read graph", out.Code)
		}
	}
	service := &graphServiceStub{err: errors.New("secret-provider-value")}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleView}}}), WithWorkbenchGraph(service))
	if err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "http://example.com/api/v1/gaggles/team/workbench/graph", nil))
	if out.Code < 500 || strings.Contains(out.Body.String(), "secret-provider-value") {
		t.Fatal("source error escaped", out.Code, out.Body)
	}
}
