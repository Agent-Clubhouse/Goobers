package httpapi

import (
	"context"
	"errors"
	"net/http"
)

// NewControlStatusHandler exposes only authenticated, instance-view status when
// the cloud read backend is not assembled. It provides no fallback data store.
func NewControlStatusHandler(auth Authenticator, status func(context.Context) (any, error), fallback http.Handler) (http.Handler, error) {
	if auth == nil || status == nil {
		return nil, errors.New("control status requires authentication and status")
	}
	if _, ok := auth.(NullAuthenticator); ok {
		return nil, errors.New("control status refuses unauthenticated transport")
	}
	if fallback != nil && !handlerAuthenticated(fallback) {
		return nil, errors.New("control status fallback requires authenticated transport")
	}
	return &controlStatusHandler{auth: auth, status: status, fallback: fallback}, nil
}

type controlStatusHandler struct {
	fallback http.Handler
	auth     Authenticator
	status   func(context.Context) (any, error)
}

func (*controlStatusHandler) authenticatedTransport() bool { return true }
func (h *controlStatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/control/status" && h.fallback != nil {
		h.fallback.ServeHTTP(w, r)
		return
	}
	principal, err := h.auth.Authenticate(r)
	if err != nil || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !principal.HasRole(RoleView) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path != "/api/v1/control/status" {
		http.Error(w, "cloud planes unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	value, err := h.status(r.Context())
	if err != nil {
		http.Error(w, "control status unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
