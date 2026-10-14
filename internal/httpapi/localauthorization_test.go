package httpapi

import (
	"net/http"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func TestLocalAdminDoesNotBroadenMachinePrincipals(t *testing.T) {
	cases := []struct {
		name, method, path, authorization string
		principal                         *Principal
		allowed                           bool
	}{
		{name: "anonymous local start", method: http.MethodPost, path: apicontract.TriggerIngestPath, allowed: true},
		{name: "unknown bearer", method: http.MethodPost, path: apicontract.TriggerIngestPath, authorization: "Bearer unrecognized"},
		{name: "pod cannot start interactive operation", method: http.MethodDelete, path: apicontract.RunsPath + "/run", principal: &Principal{Subject: "run:run", Issuer: PodPrincipalIssuer}},
		{name: "blob-scoped pod cannot trigger", method: http.MethodPost, path: apicontract.TriggerIngestPath, principal: &Principal{Subject: "run:run", Issuer: PodPrincipalIssuer, Scopes: []string{ScopeBlob}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := requestWithPrincipal(tc.method, tc.principal)
			request.URL.Path = tc.path
			request.Header.Set("Authorization", tc.authorization)
			err := LocalAdminWithScopedPrincipals().Authorize(request)
			if (err == nil) != tc.allowed {
				t.Fatalf("authorization allowed=%t, error=%v", tc.allowed, err)
			}
		})
	}
}
