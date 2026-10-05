package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/sessioning"
)

type repairRecoveryStub struct {
	calls                 int
	operation, gaggle, id string
	actor                 Principal
	value                 sessioning.PRRepairCommandView
	err                   error
}

func (s *repairRecoveryStub) Get(_ context.Context, p Principal, g, id string) (sessioning.PRRepairCommandView, error) {
	s.calls++
	s.operation, s.actor, s.gaggle, s.id = "get", p, g, id
	return s.value, s.err
}
func (s *repairRecoveryStub) Check(_ context.Context, p Principal, g, id string) (sessioning.PRRepairCommandView, error) {
	s.calls++
	s.operation, s.actor, s.gaggle, s.id = "check", p, g, id
	return s.value, s.err
}
func repairRecoveryHandlerForTest(t *testing.T, s *repairRecoveryStub, p *Principal) http.Handler {
	t.Helper()
	h, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: p}), WithPRRepairRecovery(s))
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func TestPRRepairRecoveryRouteOnlyAcceptsRetainedIdentity(t *testing.T) {
	s := &repairRecoveryStub{}
	p := &Principal{Issuer: "https://identity.example", Subject: "checker", Roles: []Role{RoleOperate}}
	h := repairRecoveryHandlerForTest(t, s, p)
	id := "repair-" + strings.Repeat("a", 32)
	base := "http://example.com/api/v1/gaggles/team/pr-repairs/" + id
	for _, tc := range []struct{ method, path, body, operation string }{{"GET", base, "", "get"}, {"POST", base + "/check", "{}", "check"}} {
		out := nativeWriteRequest(h, tc.method, tc.path, tc.body, "")
		if out.Code != 200 || out.Header().Get("Cache-Control") != "no-store" || s.operation != tc.operation || s.gaggle != "team" || s.id != id || s.actor.Subject != "checker" {
			t.Fatal(out.Code, out.Body, s)
		}
	}
	before := s.calls
	for _, tc := range []struct{ method, path, body string }{{"GET", base + "?", ""}, {"GET", base + "?source=other", ""}, {"GET", base, "{}"}, {"GET", base + "x", ""}, {"POST", base + "/check", ""}, {"POST", base + "/check", "null"}, {"POST", base + "/check", "[]"}, {"POST", base + "/check", `{"actor":"other"}`}, {"POST", base + "/check", "{} {}"}, {"POST", base + "/check", strings.Repeat(" ", 1025) + "{}"}, {"POST", base + "/check?target=foreign", "{}"}} {
		out := nativeWriteRequest(h, tc.method, tc.path, tc.body, "")
		if out.Code != 400 {
			t.Fatal(tc, out.Code, out.Body)
		}
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, func(r *http.Request) { r.Header.Set("Origin", "https://foreign.example") }} {
		r := httptest.NewRequest("POST", base+"/check", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		change(r)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		if out.Code != 400 {
			t.Fatal(out.Code, out.Body)
		}
	}
	if s.calls != before {
		t.Fatal("untrusted overrides reached service")
	}
}
func TestPRRepairRecoveryHumanScopeAndBoundedEvidence(t *testing.T) {
	base := "http://example.com/api/v1/gaggles/team/pr-repairs/repair-" + strings.Repeat("a", 32)
	for _, p := range []*Principal{nil, {Issuer: PodPrincipalIssuer, Subject: "pod", Roles: []Role{RoleAdmin}}, {Issuer: "issuer", Subject: "human", Roles: []Role{RoleView}}, {Issuer: "issuer", Subject: "human", Roles: []Role{RoleAdmin}, GeneratedChild: true}, {Issuer: "issuer", Subject: "human", Roles: []Role{RoleAdmin}, WorkflowParent: true}} {
		s := &repairRecoveryStub{}
		out := nativeWriteRequest(repairRecoveryHandlerForTest(t, s, p), "GET", base, "", "")
		if out.Code < 400 || s.calls != 0 {
			t.Fatal(out.Code, s.calls)
		}
	}
	s := &repairRecoveryStub{err: errors.New("secret-provider-value")}
	h := repairRecoveryHandlerForTest(t, s, &Principal{Issuer: "issuer", Subject: "operator", Roles: []Role{RoleOperate}})
	out := nativeWriteRequest(h, "GET", base, "", "")
	if out.Code < 500 || strings.Contains(out.Body.String(), "secret-provider-value") {
		t.Fatal(out.Code, out.Body)
	}
	s.err = nil
	s.value.Actor.Subject = strings.Repeat("x", 128<<10)
	out = nativeWriteRequest(h, "GET", base, "", "")
	if out.Code != 502 {
		t.Fatal(out.Code, out.Body)
	}
}
