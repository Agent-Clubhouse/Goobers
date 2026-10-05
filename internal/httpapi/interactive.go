package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
)

// InteractivePermissionService must require explicit gaggle membership in
// addition to the verified instance role. No request body supplies identity.
type InteractivePermissionService interface {
	InteractiveCapabilities(context.Context, Principal, string) (apicontract.InteractiveCapabilities, error)
}

// WithInteractivePermissions enables authenticated per-gaggle permission reads.
func WithInteractivePermissions(service InteractivePermissionService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("interactive permission service is required")
		}
		config.interactivePermissions = service
		return nil
	}
}

func registerInteractiveRoutes(router *Router, service InteractivePermissionService) {
	router.Handle(apicontract.RouteGaggleInteractiveCapabilities, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := PrincipalFromRequest(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "interactive_authentication_required", "interactive access requires an authenticated human")
			return
		}
		if p.Subject == "" || p.Issuer == "" || strings.HasPrefix(p.Issuer, "goobers/") || !p.HasRole(RoleView) {
			writeError(w, http.StatusForbidden, "interactive_access_denied", "interactive access is not authorized")
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "interactive_unavailable", "interactive permissions are not configured on this server")
			return
		}
		result, err := service.InteractiveCapabilities(r.Context(), p, r.PathValue("gaggle"))
		if err != nil {
			writeError(w, http.StatusForbidden, "interactive_access_denied", "interactive access is not authorized")
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
