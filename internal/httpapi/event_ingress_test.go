package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/eventing"
)

type eventIngressStub struct {
	calls           int
	binding, gaggle string
	err             error
}

func (s *eventIngressStub) PublishEvent(_ context.Context, _ Principal, g, b string, _ []byte) (apicontract.GaggleEventReceipt, error) {
	s.calls++
	s.gaggle = g
	s.binding = b
	return apicontract.GaggleEventReceipt{}, s.err
}
func (s *eventIngressStub) EventReceipt(_ context.Context, _ Principal, g, _ string) (apicontract.GaggleEventReceipt, error) {
	s.calls++
	s.gaggle = g
	return apicontract.GaggleEventReceipt{}, s.err
}

func TestGaggleEventTransportBoundsAndAuthority(t *testing.T) {
	service := &eventIngressStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://identity.example", Subject: "builds", Roles: []Role{RoleOperate}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(auth), WithGaggleEvents(service))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query, media, body string
		headers                  []string
		status                   int
	}{
		{"structured", "", "application/cloudevents+json", "{}", []string{"builds"}, 202},
		{"json", "", "application/json", "{}", []string{"builds"}, 202},
		{"missing binding", "", "application/json", "{}", nil, 400},
		{"duplicate binding", "", "application/json", "{}", []string{"builds", "other"}, 400},
		{"large binding", "", "application/json", "{}", []string{strings.Repeat("a", 129)}, 400},
		{"query", "?gaggle=other", "application/json", "{}", []string{"builds"}, 400},
		{"content type", "", "text/plain", "{}", []string{"builds"}, 400},
		{"body limit", "", "application/json", strings.Repeat("a", eventing.MaxEnvelopeBytes+1), []string{"builds"}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := service.calls
			req := httptest.NewRequest(http.MethodPost, "/api/v1/gaggles/web/events"+tc.query, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.media)
			for _, h := range tc.headers {
				req.Header.Add(apicontract.EventBindingHeader, h)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != tc.status || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(response.Code, response.Body)
			}
			if tc.status == 202 {
				if service.calls != before+1 || service.gaggle != "web" || service.binding != "builds" {
					t.Fatal("scope lost")
				}
			} else if service.calls != before {
				t.Fatal("invalid request reached ledger")
			}
		})
	}
	for _, p := range []*Principal{nil, {Issuer: PodPrincipalIssuer, Subject: "stage", Roles: []Role{RoleAdmin}}, {Issuer: "https://identity.example", Subject: "builds", Roles: []Role{RoleAdmin}, Scopes: []string{"run"}}, {Issuer: "https://identity.example", Subject: "builds", Roles: []Role{RoleAdmin}, WorkflowParent: true}} {
		auth.principal = p
		before := service.calls
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/gaggles/web/events", nil))
		if response.Code < 400 || service.calls != before {
			t.Fatal("internal or anonymous authority admitted", response.Code)
		}
	}
	auth.principal = &Principal{Issuer: "https://identity.example", Subject: "builds", Roles: []Role{RoleOperate}}
	service.err = NewInterventionError(429, "limited", "retry", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gaggles/web/events", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apicontract.EventBindingHeader, "builds")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 429 || response.Header().Get("Retry-After") != "1" {
		t.Fatal(response.Code, response.Header())
	}
}

func TestGaggleEventLoopbackDoesNotCreateProducerIdentity(t *testing.T) {
	service := &eventIngressStub{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithGaggleEvents(service))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/gaggles/web/events", nil))
	if response.Code != 401 || service.calls != 0 {
		t.Fatal(response.Code, service.calls)
	}
}
