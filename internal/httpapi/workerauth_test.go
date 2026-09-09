package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func TestWorkerIdentityIsConfinedToConfigDigestRead(t *testing.T) {
	paths := []string{apicontract.ConfigDigestPath, RunsPath, EventsPath, HealthPath, apicontract.ConfigDigestPath + "/", "/api/v1/unknown"}
	for _, route := range podRouteTable() {
		paths = append(paths, route.path)
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			request := httptest.NewRequest(method, path, nil)
			// Even accidentally supplied roles/scopes cannot widen this identity.
			principal := Principal{Subject: "worker:dispatcher", Issuer: WorkerPrincipalIssuer, Roles: []Role{RoleAdmin}, Scopes: knownPodScopes()}
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
			err := RequireRoles().Authorize(request)
			want := method == http.MethodGet && path == apicontract.ConfigDigestPath
			if (err == nil) != want {
				t.Fatalf("worker admitted %s %s=%v, want %v", method, path, err == nil, want)
			}
		}
	}
}
