// Package httpapi exposes the versioned, read-only loopback HTTP adapter over
// readservice.Reader.
package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/readservice"
)

const (
	// Prefix is the versioned root for all HTTP API routes.
	Prefix = "/api/v1"
	// HealthPath is the daemon health endpoint.
	HealthPath = Prefix + "/health"
)

// Authorizer preserves the authorization boundary for every API route. Tier 1
// supplies AllowAll; later tiers can replace it without changing handlers.
type Authorizer interface {
	Authorize(*http.Request) error
}

type authorizerFunc func(*http.Request) error

func (f authorizerFunc) Authorize(r *http.Request) error { return f(r) }

// AllowAll is the tier-1 local-trust authorizer.
var AllowAll Authorizer = authorizerFunc(func(*http.Request) error { return nil })

// ErrorEnvelope is the single error shape returned by every API route.
type ErrorEnvelope struct {
	Error APIError `json:"error"`
}

// APIError is a stable machine code and safe human-readable message.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Router registers read-only routes behind an Authorizer.
type Router struct {
	mux        *http.ServeMux
	authorizer Authorizer
}

// NewRouter constructs an empty API router.
func NewRouter(authorizer Authorizer) (*Router, error) {
	if authorizer == nil {
		return nil, errors.New("http API authorizer is required")
	}
	return &Router{mux: http.NewServeMux(), authorizer: authorizer}, nil
}

// HandleGET registers a read route. Other methods receive the structured error
// envelope rather than net/http's plain-text method error.
func (r *Router) HandleGET(path string, handler http.HandlerFunc) {
	r.mux.HandleFunc(path, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if err := r.authorizer.Authorize(request); err != nil {
			writeError(w, http.StatusForbidden, "forbidden", "request is not authorized")
			return
		}
		handler(w, request)
	})
}

// Handler returns the registered routes with a structured unknown-route
// fallback.
func (r *Router) Handler() http.Handler {
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		handler, pattern := r.mux.Handler(request)
		if pattern == "" {
			notFound.ServeHTTP(w, request)
			return
		}
		handler.ServeHTTP(w, request)
	})
}

// NewHandler registers the v1 read routes over the shared service.
func NewHandler(reader readservice.Reader, authorizer Authorizer, errorLog *log.Logger) (http.Handler, error) {
	if reader == nil {
		return nil, errors.New("http API read service is required")
	}
	if errorLog == nil {
		return nil, errors.New("http API error logger is required")
	}
	router, err := NewRouter(authorizer)
	if err != nil {
		return nil, err
	}
	router.HandleGET(HealthPath, func(w http.ResponseWriter, request *http.Request) {
		health, err := reader.Health(request.Context())
		if err != nil {
			errorLog.Printf("health read failed: %v", err)
			writeError(w, http.StatusInternalServerError, "read_error", "runtime state could not be read")
			return
		}
		writeJSON(w, http.StatusOK, health)
	})
	return router.Handler(), nil
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorEnvelope{Error: APIError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
