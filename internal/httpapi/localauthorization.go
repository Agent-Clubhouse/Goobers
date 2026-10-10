package httpapi

import (
	"net/http"
	"strings"
)

// LocalAdminWithScopedPrincipals preserves anonymous administration only for a
// loopback-bound daemon. Authenticated machine requests still obey RequireRoles;
// a supplied but unrecognized credential cannot fall back to local admin.
// Never use this authorizer on a remotely reachable listener.
func LocalAdminWithScopedPrincipals() Authorizer {
	scoped := RequireRoles()
	return authorizerFunc(func(request *http.Request) error {
		if _, ok := PrincipalFromRequest(request); ok || strings.TrimSpace(request.Header.Get("Authorization")) != "" {
			return scoped.Authorize(request)
		}
		return nil
	})
}
