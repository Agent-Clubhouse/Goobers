package httpapi

import "net/http"

// routeHandler serves one exact path itself and forwards everything else,
// including the authenticatedTransport() and shutdown() side-channels Server
// relies on, to inner untouched.
type routeHandler struct {
	inner http.Handler
	path  string
	route http.Handler
}

// WrapWithRoute mounts route at the literal path ahead of inner. The route
// authenticates itself (for example an HMAC-signed webhook), so it bypasses
// inner's bearer authentication; every other request, and NewServer's
// off-loopback authentication gate, see inner exactly as if the wrapper did
// not exist. A plain http.ServeMux cannot do this: it hides inner's
// authenticated-transport evidence and would make a fully authenticated API
// look open.
func WrapWithRoute(inner http.Handler, path string, route http.Handler) http.Handler {
	return &routeHandler{inner: inner, path: path, route: route}
}

func (h *routeHandler) authenticatedTransport() bool { return handlerAuthenticated(h.inner) }

func (h *routeHandler) shutdown() {
	if lifecycle, ok := h.inner.(interface{ shutdown() }); ok {
		lifecycle.shutdown()
	}
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == h.path {
		h.route.ServeHTTP(w, r)
		return
	}
	h.inner.ServeHTTP(w, r)
}
