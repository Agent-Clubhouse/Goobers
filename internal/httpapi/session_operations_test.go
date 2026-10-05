package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

type operationFixture struct{ calls int }

func (f *operationFixture) AuthenticateSessionOperation(token string) (string, error) {
	if token != sessioning.OperationTokenPrefix+strings.Repeat("a", 64) {
		return "", errors.New("invalid")
	}
	return "run-one", nil
}
func (f *operationFixture) GetBacklogItem(_ context.Context, token, run string, r sessioning.BacklogReadRequest) (workbench.BacklogItem, error) {
	f.calls++
	if run != "run-one" || r.SourceBindingID != "issues" || r.ID != "42" {
		return workbench.BacklogItem{}, errors.New("binding mismatch")
	}
	return workbench.BacklogItem{Title: "Scoped item"}, nil
}
func (f *operationFixture) ListBacklogItems(context.Context, string, string, sessioning.BacklogListRequest) (workbench.BacklogPage, error) {
	f.calls++
	return workbench.BacklogPage{Items: []workbench.BacklogItem{}, Exhausted: true}, nil
}
func TestSessionOperationRouteConfinesOpaqueGrant(t *testing.T) {
	service := &operationFixture{}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(DenyAllAuthenticator{}), WithSessionOperations(service))
	if err != nil {
		t.Fatal(err)
	}
	token := sessioning.OperationTokenPrefix + strings.Repeat("a", 64)
	for _, tc := range []struct {
		path, body, token string
		status            int
	}{
		{"/api/v1/runs/run-one/session-operations/get_backlog_item", `{"sourceBindingId":"issues","id":"42"}`, token, 200},
		{"/api/v1/runs/run-two/session-operations/get_backlog_item", `{"sourceBindingId":"issues","id":"42"}`, token, 403},
		{"/api/v1/runs/run-one/session-operations/get_backlog_item", `{"sourceBindingId":"issues","id":"42","actor":"admin"}`, token, 400},
		{"/api/v1/runs/run-one/session-operations/get_backlog_item", `{"sourceBindingId":"issues","id":"42","id":"43"}`, token, 400},
		{"/api/v1/runs/run-one/session-operations/get_backlog_item", `{"sourceBindingId":"issues","id":"42"}`, token + "invalid", 401},
		{"/api/v1/runs/run-one/cancel", `{}`, token, 403},
		{"/api/v1/credentials/resolve", `{}`, token, 403},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tc.token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Errorf("%s status=%d want=%d %s", tc.path, response.Code, tc.status, response.Body.String())
		}
	}
	if service.calls != 1 {
		t.Fatal("invalid calls escaped handler", service.calls)
	}
}
func TestSessionOperationRoutesAdvertiseInstallation(t *testing.T) {
	for _, id := range []apicontract.RouteID{apicontract.RouteSessionBacklogRead, apicontract.RouteSessionBacklogList} {
		if enabled, _, _ := routeAvailability(id, handlerConfig{}); enabled {
			t.Fatal("uninstalled source tools advertised")
		}
		if enabled, _, _ := routeAvailability(id, handlerConfig{sessionOperations: &operationFixture{}}); !enabled {
			t.Fatal("installed source tools unavailable")
		}
	}
}
