package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type permissionStub struct {
	calls     int
	principal Principal
	gaggle    string
	err       error
}

func (s *permissionStub) InteractiveCapabilities(_ context.Context, p Principal, gaggle string) (apicontract.InteractiveCapabilities, error) {
	s.calls++
	s.principal = p
	s.gaggle = gaggle
	return apicontract.InteractiveCapabilities{Gaggle: gaggle, SourceWriteMode: "pull-request", Actions: []apicontract.InteractiveActionPermission{}}, s.err
}
func TestInteractivePermissionRouteAuthenticatesEvenOnLoopback(t *testing.T) {
	service := &permissionStub{}
	auth := &fakeAuthenticator{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithAuthenticator(auth), WithInteractivePermissions(service))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		principal *Principal
		status    int
	}{
		{"anonymous", nil, http.StatusUnauthorized}, {"instance role missing", &Principal{Issuer: "https://identity.example", Subject: "alice"}, http.StatusForbidden}, {"human", &Principal{Issuer: "https://identity.example", Subject: "alice", Groups: []string{"team"}, Roles: []Role{RoleView}}, http.StatusOK},
		{"pod", &Principal{Issuer: PodPrincipalIssuer, Subject: "alice", Roles: []Role{RoleAdmin}}, http.StatusForbidden}, {"worker", &Principal{Issuer: WorkerPrincipalIssuer, Subject: "alice", Roles: []Role{RoleAdmin}}, http.StatusForbidden}, {"child grant", &Principal{Issuer: ChildWorkflowPrincipalIssuer, Subject: "alice", Roles: []Role{RoleAdmin}}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth.principal = tc.principal
			before := service.calls
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/gaggles/web/interactive-capabilities", nil))
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			if tc.status != http.StatusOK && service.calls != before {
				t.Fatal("nonhuman reached permission service")
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("personal permissions can be cached")
			}
		})
	}
	if service.gaggle != "web" || len(service.principal.Groups) != 1 {
		t.Fatalf("identity not forwarded: %+v", service)
	}
	service.err = errors.New("secret internal error")
	auth.principal = &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleAdmin}}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/gaggles/web/interactive-capabilities", nil))
	if response.Code != http.StatusForbidden {
		t.Fatal(response.Code)
	}
}
