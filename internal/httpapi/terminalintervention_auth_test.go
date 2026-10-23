package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func TestTerminalInterventionIdentityCannotReachOtherPlanes(t *testing.T) {
	for _, route := range apicontract.V1Routes() {
		path := strings.ReplaceAll(route.Path, "{run}", "run-1")
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			request := httptest.NewRequest(method, path, nil)
			principal := Principal{Subject: "worker:engine", Issuer: WorkerTerminalInterventionPrincipalIssuer, Roles: []Role{RoleAdmin}, Scopes: knownPodScopes()}
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
			err := RequireRoles().Authorize(request)
			want := route.ID == apicontract.RouteJournalTerminalIntervention && method == http.MethodPost
			if (err == nil) != want {
				t.Fatalf("terminal worker admitted %s %s=%v, want %v", method, path, err == nil, want)
			}
		}
	}
}
