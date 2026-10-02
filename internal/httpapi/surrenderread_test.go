package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type boundedSurrenderFixture struct {
	fakeSurrenderService
	seen  bool
	data  []byte
	limit int64
}

func (f *boundedSurrenderFixture) Has(context.Context, string, string, int) (bool, error) {
	return f.seen, nil
}
func (f *boundedSurrenderFixture) GetBounded(_ context.Context, _ string, _ string, _ int, limit int64) ([]byte, error) {
	f.limit = limit
	return f.data, nil
}

func TestSurrenderReadRequiresDedicatedWorkerEvenWithAllowAll(t *testing.T) {
	for _, issuer := range []string{"human", PodPrincipalIssuer, WorkerPrincipalIssuer, WorkerBlobPrincipalIssuer, CredentialGrantPrincipalIssuer, ""} {
		auth := &fakeAuthenticator{principal: &Principal{Subject: "identity", Issuer: issuer, Roles: []Role{RoleAdmin}, Scopes: knownPodScopes()}}
		handler := writePlaneHandler(t, auth, AllowAll, WithSurrenderService(&boundedSurrenderFixture{}))
		for _, suffix := range []string{"", "/seen"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, surrenderPath("r", "s", 1)+suffix, nil))
			if response.Code != http.StatusForbidden {
				t.Fatalf("issuer %q read %s: %d", issuer, suffix, response.Code)
			}
		}
	}
}

func TestSurrenderReadBudgetsAndPresence(t *testing.T) {
	plane := &boundedSurrenderFixture{}
	auth := &fakeAuthenticator{principal: &Principal{Subject: "worker:node", Issuer: WorkerSurrenderPrincipalIssuer}}
	handler := writePlaneHandler(t, auth, RequireRoles(), WithSurrenderService(plane))
	get := func(suffix string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, surrenderPath("r", "s", 1)+suffix, nil))
		return r
	}
	if r := get("/seen"); r.Code != 200 || !strings.Contains(r.Body.String(), `"seen":false`) {
		t.Fatalf("absent presence: %d %s", r.Code, r.Body)
	}
	if r := get(""); r.Code != 404 {
		t.Fatalf("absent result: %d", r.Code)
	}
	plane.seen = true
	plane.data = []byte(surrenderedBody("success"))
	if r := get(""); r.Code != 200 || r.Body.String() != string(plane.data) || plane.limit != maxSurrenderBody || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("read: %d %s limit=%d", r.Code, r.Body, plane.limit)
	}
	plane.data = []byte(strings.Repeat("x", maxSurrenderBody+1))
	if r := get(""); r.Code != 500 || r.Body.Len() > 1024 {
		t.Fatalf("unbounded response: %d bytes=%d", r.Code, r.Body.Len())
	}
}

func TestSurrenderReadDiscoveryRequiresReadableBackend(t *testing.T) {
	for _, backend := range []struct {
		name    string
		service SurrenderService
		want    bool
	}{
		{"absent", nil, false}, {"write-only", &fakeSurrenderService{}, false}, {"readable", &boundedSurrenderFixture{}, true},
	} {
		t.Run(backend.name, func(t *testing.T) {
			for _, route := range []apicontract.RouteID{apicontract.RouteStageSurrenderGet, apicontract.RouteStageSurrenderSeen} {
				available, _, _ := routeAvailability(route, handlerConfig{surrenders: backend.service})
				if available != backend.want {
					t.Fatalf("%s available=%v want=%v", route, available, backend.want)
				}
			}
		})
	}
}
