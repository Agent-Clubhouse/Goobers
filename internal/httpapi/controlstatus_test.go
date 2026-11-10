package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type controlStatusAuth struct {
	principal *Principal
	err       error
}

func (a controlStatusAuth) Authenticate(*http.Request) (*Principal, error) { return a.principal, a.err }

func TestControlStatusFailsClosedAndContainsPodTokens(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal *Principal
		status    int
	}{
		{"anonymous", nil, http.StatusUnauthorized},
		{"pod", &Principal{Subject: "run:r", Issuer: PodPrincipalIssuer}, http.StatusForbidden},
		{"viewer", &Principal{Subject: "human", Roles: []Role{RoleView}}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler, err := NewControlStatusHandler(controlStatusAuth{principal: tc.principal}, func(context.Context) (any, error) { called = true; return map[string]bool{"ready": false}, nil }, nil)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/control/status", nil))
			if recorder.Code != tc.status || called != (tc.status == http.StatusOK) {
				t.Fatalf("status=%d called=%v", recorder.Code, called)
			}
		})
	}
	if _, err := NewControlStatusHandler(NullAuthenticator{}, func(context.Context) (any, error) { return nil, nil }, nil); err == nil {
		t.Fatal("accepted NullAuthenticator")
	}
}
