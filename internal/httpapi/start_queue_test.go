package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type startQueueStub struct {
	calls      int
	gaggle, id string
}

func (s *startQueueStub) StartQueue(_ context.Context, _ Principal, g, c string, l int) (apicontract.StartQueuePage, error) {
	s.calls++
	s.gaggle = g
	return apicontract.StartQueuePage{Gaggle: g, Items: []apicontract.StartQueueItem{}}, nil
}
func (s *startQueueStub) StartQueueItem(_ context.Context, _ Principal, g, id string) (apicontract.StartQueueItem, error) {
	s.calls++
	s.gaggle = g
	s.id = id
	return apicontract.StartQueueItem{}, nil
}
func (s *startQueueStub) CancelQueuedStart(_ context.Context, _ Principal, g, id string, _ apicontract.StartQueueCancelInput) (apicontract.StartQueueItem, error) {
	s.calls++
	s.gaggle = g
	s.id = id
	return apicontract.StartQueueItem{}, nil
}
func TestStartQueueTransportClosedAuthorityAndBounds(t *testing.T) {
	service := &startQueueStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://identity", Subject: "alice", Roles: []Role{RoleOperate}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(auth), WithStartQueue(service))
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/gaggles/web/start-queue"
	item := base + "/trigger-0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{{"GET", base + "?limit=50", "", 200}, {"GET", item, "", 200}, {"POST", item + "/cancel", `{"requestId":"cancel","reason":"No longer needed"}`, 202}, {"GET", base + "?limit=51", "", 400}, {"GET", base + "?limit=1&limit=2", "", 400}, {"GET", item + "?actor=admin", "", 400}, {"POST", item + "/cancel", `{"requestId":"c","reason":"x","actor":"admin"}`, 400}, {"POST", item + "/cancel", `{"requestId":"c","requestId":"d","reason":"x"}`, 400}, {"POST", item + "/cancel", `{"requestId":"c","reason":"x"} {}`, 400}, {"POST", item + "/cancel", strings.Repeat("x", 4097), 400}} {
		before := service.calls
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(tc, w.Code, w.Body)
		}
		if tc.status < 400 && service.calls != before+1 || tc.status >= 400 && service.calls != before {
			t.Fatal("invalid dispatch", tc, service.calls)
		}
	}
	for _, principal := range []*Principal{nil, {Issuer: PodPrincipalIssuer, Subject: "stage", Roles: []Role{RoleAdmin}}, {Issuer: "https://identity", Subject: "alice", Roles: []Role{RoleAdmin}, Scopes: []string{"run"}}, {Issuer: "https://identity", Subject: "alice", Roles: []Role{RoleAdmin}, WorkflowParent: true}} {
		auth.principal = principal
		before := service.calls
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", base, nil))
		if w.Code < 400 || service.calls != before {
			t.Fatal("internal authority borrowed human", w.Code)
		}
	}
	loopback, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithStartQueue(service))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	loopback.ServeHTTP(w, httptest.NewRequest("POST", item+"/cancel", nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
