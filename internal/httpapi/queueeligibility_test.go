package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQueueEligibilityRouteAuthorizesBeforeReading(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal *Principal
		status    int
	}{
		{"viewer", &Principal{Subject: "viewer", Roles: []Role{RoleView}}, http.StatusOK},
		{"no role", &Principal{Subject: "unprivileged"}, http.StatusForbidden},
		{"pod", &Principal{Subject: "run:other", Issuer: PodPrincipalIssuer, Roles: []Role{RoleView}}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeReader{}
			handler, err := NewHandler(reader, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: tc.principal}))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/gaggles/team/workflows/review/queue-eligibility", nil))
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tc.status == http.StatusOK {
				if reader.called != 1 || reader.lastGaggle != "team" || reader.lastWorkflow != "review" {
					t.Fatalf("scope lost or duplicate reads: %+v", reader)
				}
			} else if reader.called != 0 {
				t.Fatalf("unauthorized request reached reader: %d", reader.called)
			}
		})
	}
}

func TestQueueEligibilityRouteRejectsInvalidScopeBeforeReading(t *testing.T) {
	reader := &fakeReader{}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/gaggles/invalid_name/workflows/review/queue-eligibility", nil))
	if response.Code != http.StatusBadRequest || reader.called != 0 {
		t.Fatalf("invalid scope reached reader: status=%d calls=%d", response.Code, reader.called)
	}
}
