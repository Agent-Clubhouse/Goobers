package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type publicationCheckStub struct {
	calls     int
	principal Principal
	run, key  string
}

func (s *publicationCheckStub) CheckChildPublication(_ context.Context, p Principal, run, key string, _ apicontract.ChildPublicationCheckRequest) (apicontract.ChildPublicationCheckResult, error) {
	s.calls++
	s.principal, s.run, s.key = p, run, key
	return apicontract.ChildPublicationCheckResult{RunID: run, RequestID: key}, nil
}
func TestChildPublicationCheckUsesHumanIdentityAndExactBody(t *testing.T) {
	service := &publicationCheckStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleOperate}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(auth), WithChildPublicationChecks(service))
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"action":"pr","expectedIntentDigest":"sha256:` + strings.Repeat("a", 64) + `"}`
	for _, test := range []struct {
		name, body, key, origin, query string
		code                           int
	}{
		{"valid", valid, "request-one", "", "", 200},
		{"no key", valid, "", "", "", 400},
		{"foreign origin", valid, "two", "https://other.example", "", 400},
		{"unknown field", strings.TrimSuffix(valid, "}") + `,"gaggle":"other"}`, "three", "", "", 400},
		{"wrong digest", `{"action":"pr","expectedIntentDigest":"other"}`, "four", "", "", 400},
		{"wrong action", strings.Replace(valid, `"pr"`, `"push"`, 1), "five", "", "", 400},
		{"query", valid, "six", "", "?gaggle=other", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := service.calls
			request := httptest.NewRequest(http.MethodPost, "http://example.com/api/v1/runs/child/child-publications/check"+test.query, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", test.key)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.code || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(response.Code, response.Body)
			}
			if test.code != 200 && service.calls != before {
				t.Fatal("invalid request reached observer")
			}
		})
	}
	if service.principal.Subject != "alice" || service.run != "child" || service.key != "request-one" {
		t.Fatal(service)
	}
	for _, principal := range []Principal{
		{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleView}},
		{Issuer: PodPrincipalIssuer, Subject: "alice", Roles: []Role{RoleAdmin}},
	} {
		auth.principal = &principal
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/child/child-publications/check", strings.NewReader(valid))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "reject")
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || service.calls != 1 {
			t.Fatal(response.Code, service.calls)
		}
	}
}
