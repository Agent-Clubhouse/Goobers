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

func TestTerminalInterventionRouteAuthorizerRequiresItsOwnIssuer(t *testing.T) {
	// Exercise the authorizer alone: the handler's independent check must not
	// conceal a regression in this earlier trust boundary.
	for _, issuer := range []string{"", PodPrincipalIssuer, WorkerPrincipalIssuer, WorkerChildAdmissionPrincipalIssuer, WorkerTerminalInterventionPrincipalIssuer} {
		t.Run(issuer, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/run-1/journal/terminal-intervention", nil)
			principal := Principal{Subject: "actor", Issuer: issuer, Roles: []Role{RoleAdmin}, Scopes: knownPodScopes()}
			handled, err := authorizeIssuerBoundRoute(request, principal)
			if issuer == WorkerTerminalInterventionPrincipalIssuer {
				if handled || err != nil {
					t.Fatalf("dedicated issuer should proceed to its route rule: handled=%v err=%v", handled, err)
				}
			} else if !handled || err == nil {
				t.Fatalf("wrong issuer passed route boundary: handled=%v err=%v", handled, err)
			}
		})
	}
}
