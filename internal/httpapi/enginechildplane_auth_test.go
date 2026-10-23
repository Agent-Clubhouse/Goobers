package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func TestWorkerChildAdmissionIdentityIsConfinedToItsPlane(t *testing.T) {
	plane := map[string]bool{apicontract.EngineChildAdmitPath: true, apicontract.EngineChildLoadPath: true, apicontract.EngineChildCancelPath: true}
	paths := []string{apicontract.ConfigDigestPath, RunsPath, EventsPath, HealthPath, apicontract.EngineChildAdmitPath + "/", "/api/v1/unknown"}
	for _, route := range apicontract.V1Routes() {
		paths = append(paths, route.Path)
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			request := httptest.NewRequest(method, path, nil)
			// Accidentally supplied roles or scopes cannot widen this identity.
			principal := Principal{Subject: "worker:engine", Issuer: WorkerChildAdmissionPrincipalIssuer, Roles: []Role{RoleAdmin}, Scopes: knownPodScopes()}
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
			err := RequireRoles().Authorize(request)
			if want := method == http.MethodPost && plane[path]; (err == nil) != want {
				t.Fatalf("worker admitted %s %s=%v, want %v", method, path, err == nil, want)
			}
		}
	}
	// Stage pods, even with every scope, never reach the plane.
	for path := range plane {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		principal := Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer, Scopes: knownPodScopes()}
		request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
		if RequireRoles().Authorize(request) == nil {
			t.Fatalf("pod principal reached %s", path)
		}
	}
}
