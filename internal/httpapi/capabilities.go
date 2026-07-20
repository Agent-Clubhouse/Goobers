package httpapi

import (
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
)

// SurfaceActions returns classifications from the routes registered by the
// production handler assembly.
func SurfaceActions() []apicontract.SurfaceAction {
	router := &Router{
		mux:           http.NewServeMux(),
		authenticator: NullAuthenticator{},
		authorizer:    AllowAll,
	}
	registerV1Routes(router, nil, nil)
	// The events route is part of the versioned surface even though its stream
	// is optional wiring. Only the registration is probed here — the handler
	// closure is never invoked — so a nil stream is safe.
	registerEventRoute(router, nil)
	actions := make([]apicontract.SurfaceAction, 0, len(router.routes))
	for _, route := range router.routes {
		actions = append(actions, route.SurfaceAction())
	}
	return actions
}
