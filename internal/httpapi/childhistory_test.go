package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

func TestChildHistoryReadAuthorizationAndBadCursor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal Principal
		want      int
	}{
		{"viewer", Principal{Subject: "viewer", Roles: []Role{RoleView}}, http.StatusOK},
		{"unmapped", Principal{Subject: "unmapped"}, http.StatusForbidden},
		{"pod", Principal{Subject: "parent", Issuer: PodPrincipalIssuer}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeReader{}
			handler, err := NewHandler(reader, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: &tc.principal}))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.RunsPath+"/parent/children", nil))
			if response.Code != tc.want {
				t.Fatalf("status %d: %s", response.Code, response.Body)
			}
			if tc.want != http.StatusOK && reader.runID != "" {
				t.Fatal("unauthorized request reached reader")
			}
		})
	}
	reader := &fakeReader{err: readservice.ErrInvalidCursor}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.RunsPath+"/parent/children?cursor=bad", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", response.Code, response.Body)
	}
}
