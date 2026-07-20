// Package httpapi exposes the versioned, read-only loopback HTTP adapter over
// readservice.Reader.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"slices"
	"strconv"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

const (
	// Prefix is the versioned root for all HTTP API routes.
	Prefix = apicontract.V1Prefix
	// HealthPath is the daemon health endpoint.
	HealthPath = apicontract.HealthPath
	// TelemetryStatsPath exposes workflow and stage telemetry aggregates.
	TelemetryStatsPath = apicontract.TelemetryStatsPath
	// TelemetryErrorsPath exposes paginated recent telemetry errors.
	TelemetryErrorsPath = apicontract.TelemetryErrorsPath
	// RunsPath is the run history endpoint.
	RunsPath = apicontract.RunsPath
	// InstancePath is the instance inventory endpoint.
	InstancePath = apicontract.InstancePath
	// GagglesPath is the gaggle inventory endpoint.
	GagglesPath = apicontract.GagglesPath
	// GaggleGoobersPath is the gaggle-scoped goober inventory route.
	GaggleGoobersPath = apicontract.GaggleGoobersPath
	// GaggleWorkflowsPath is the gaggle-scoped workflow inventory route.
	GaggleWorkflowsPath = apicontract.GaggleWorkflowsPath
	// WorkflowDetailPath is the gaggle-scoped workflow detail route.
	WorkflowDetailPath = apicontract.WorkflowDetailPath
	// EventsPath is the resumable SSE read-model invalidation stream.
	EventsPath = apicontract.EventsPath
)

// Authorizer preserves the authorization boundary for every API route. Tier 1
// supplies AllowAll; later tiers can replace it without changing handlers.
type Authorizer interface {
	Authorize(*http.Request) error
}

// Principal is the identity established by an Authenticator.
type Principal struct {
	Subject string
}

// Authenticator establishes the caller identity before authorization.
type Authenticator interface {
	Authenticate(*http.Request) (*Principal, error)
}

// NullAuthenticator is the tier-1 local-trust authenticator. It requires no
// identity and leaves the request anonymous.
type NullAuthenticator struct{}

// Authenticate accepts anonymous local requests.
func (NullAuthenticator) Authenticate(*http.Request) (*Principal, error) { return nil, nil }

type principalContextKey struct{}

// PrincipalFromRequest returns the identity established for a request.
func PrincipalFromRequest(request *http.Request) (Principal, bool) {
	if request == nil {
		return Principal{}, false
	}
	principal, ok := request.Context().Value(principalContextKey{}).(Principal)
	return principal, ok
}

type authorizerFunc func(*http.Request) error

func (f authorizerFunc) Authorize(r *http.Request) error { return f(r) }

// AllowAll is the tier-1 local-trust authorizer.
var AllowAll Authorizer = authorizerFunc(func(*http.Request) error { return nil })

// ErrorEnvelope is the single error shape returned by every API route.
type ErrorEnvelope = apicontract.ErrorEnvelope

// APIError is a stable machine code and safe human-readable message.
type APIError = apicontract.APIError

// Router registers read-only routes behind an Authorizer.
type Router struct {
	mux           *http.ServeMux
	authenticator Authenticator
	authorizer    Authorizer
	routes        []apicontract.Route
}

type handlerConfig struct {
	events        *EventStream
	authenticator Authenticator
}

// HandlerOption configures optional HTTP transport surfaces.
type HandlerOption func(*handlerConfig) error

// WithEventStream registers the resumable SSE invalidation endpoint.
func WithEventStream(stream *EventStream) HandlerOption {
	return func(config *handlerConfig) error {
		if stream == nil {
			return errors.New("http API event stream is required")
		}
		config.events = stream
		return nil
	}
}

// WithAuthenticator replaces the tier-1 NullAuthenticator.
func WithAuthenticator(authenticator Authenticator) HandlerOption {
	return func(config *handlerConfig) error {
		if authenticator == nil {
			return errors.New("http API authenticator is required")
		}
		config.authenticator = authenticator
		return nil
	}
}

type apiHandler struct {
	http.Handler
	events *EventStream
}

func (h *apiHandler) shutdown() {
	if h.events != nil {
		h.events.Close()
	}
}

// NewRouter constructs an empty API router.
func NewRouter(authorizer Authorizer) (*Router, error) {
	return newRouter(NullAuthenticator{}, authorizer)
}

func newRouter(authenticator Authenticator, authorizer Authorizer) (*Router, error) {
	if authenticator == nil {
		return nil, errors.New("http API authenticator is required")
	}
	if authorizer == nil {
		return nil, errors.New("http API authorizer is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	return &Router{mux: mux, authenticator: authenticator, authorizer: authorizer}, nil
}

// Handle registers a typed contract route. Other methods receive the structured
// error envelope rather than net/http's plain-text method error.
func (r *Router) Handle(routeID apicontract.RouteID, handler http.HandlerFunc) {
	route, ok := apicontract.V1Route(routeID)
	if !ok {
		panic(fmt.Sprintf("unknown API route ID %q", routeID))
	}
	r.routes = append(r.routes, route)
	r.mux.HandleFunc(route.Path, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != route.Method {
			w.Header().Set("Allow", route.Method)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		principal, err := r.authenticator.Authenticate(request)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "request is not authenticated")
			return
		}
		if principal != nil {
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, *principal))
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
		if request.URL.Path != path.Clean(request.URL.Path) {
			notFound.ServeHTTP(w, request)
			return
		}
		_, pattern := r.mux.Handler(request)
		if pattern == "" {
			notFound.ServeHTTP(w, request)
			return
		}
		r.mux.ServeHTTP(w, request)
	})
}

// NewHandler registers the v1 read routes over the shared service.
func NewHandler(reader readservice.Reader, authorizer Authorizer, errorLog *log.Logger, opts ...HandlerOption) (http.Handler, error) {
	if reader == nil {
		return nil, errors.New("http API read service is required")
	}
	if errorLog == nil {
		return nil, errors.New("http API error logger is required")
	}
	config := handlerConfig{authenticator: NullAuthenticator{}}
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return nil, err
		}
	}
	router, err := newRouter(config.authenticator, authorizer)
	if err != nil {
		return nil, err
	}
	registerV1Routes(router, reader, errorLog)
	// The event stream is optional wiring, so the events route is only part of
	// what this handler must serve when a stream is actually configured.
	expected := apicontract.V1Routes()
	if config.events != nil {
		registerEventRoute(router, config.events)
	} else {
		expected = slices.DeleteFunc(expected, func(route apicontract.Route) bool {
			return route.ID == apicontract.RouteEvents
		})
	}
	if err := apicontract.ValidateRoutes(expected, router.routes); err != nil {
		return nil, fmt.Errorf("register HTTP API routes: %w", err)
	}
	return &apiHandler{Handler: router.Handler(), events: config.events}, nil
}

func registerV1Routes(router *Router, reader readservice.Reader, errorLog *log.Logger) {
	router.Handle(apicontract.RouteHealth, func(w http.ResponseWriter, request *http.Request) {
		health, err := reader.Health(request.Context())
		if err != nil {
			errorLog.Printf("health read failed: %v", err)
			writeError(w, http.StatusInternalServerError, "read_error", "runtime state could not be read")
			return
		}
		writeJSON(w, http.StatusOK, health)
	})
	registerTelemetryRoutes(router, reader, errorLog)
	registerRunRoutes(router, reader, errorLog)
	registerInventoryRoutes(router, reader, errorLog)
}

func registerRunRoutes(router *Router, reader readservice.Reader, errorLog *log.Logger) {
	router.Handle(apicontract.RouteRuns, func(w http.ResponseWriter, request *http.Request) {
		options, err := runListOptions(request)
		if err != nil {
			writeReadError(w, errorLog, "list runs", err)
			return
		}
		runs, err := reader.ListRuns(request.Context(), options)
		if err != nil {
			writeReadError(w, errorLog, "list runs", err)
			return
		}
		writeJSON(w, http.StatusOK, runs)
	})
	router.Handle(apicontract.RouteRunDetail, func(w http.ResponseWriter, request *http.Request) {
		run, err := reader.GetRun(request.Context(), request.PathValue("run"))
		if err != nil {
			writeReadError(w, errorLog, "get run", err)
			return
		}
		writeJSON(w, http.StatusOK, run)
	})
	router.Handle(apicontract.RouteRunEvents, func(w http.ResponseWriter, request *http.Request) {
		events, err := reader.RunEvents(request.Context(), request.PathValue("run"))
		if err != nil {
			writeReadError(w, errorLog, "read run events", err)
			return
		}
		writeJSON(w, http.StatusOK, events)
	})
	router.Handle(apicontract.RouteStageAttempts, func(w http.ResponseWriter, request *http.Request) {
		attempts, err := reader.StageAttempts(
			request.Context(),
			request.PathValue("run"),
			request.PathValue("stage"),
		)
		if err != nil {
			writeReadError(w, errorLog, "read stage attempts", err)
			return
		}
		writeJSON(w, http.StatusOK, attempts)
	})
	router.Handle(apicontract.RouteRunArtifact, func(w http.ResponseWriter, request *http.Request) {
		artifact, err := reader.Artifact(
			request.Context(),
			request.PathValue("run"),
			request.PathValue("digest"),
		)
		if err != nil {
			writeReadError(w, errorLog, "read artifact", err)
			return
		}
		w.Header().Set("Content-Type", artifact.Metadata.MediaType)
		w.Header().Set("Content-Length", strconv.Itoa(len(artifact.Bytes)))
		w.Header().Set("ETag", `"`+artifact.Metadata.Digest+`"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Goobers-Digest", artifact.Metadata.Digest)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(artifact.Bytes)
	})
}

func runListOptions(request *http.Request) (readservice.RunListOptions, error) {
	query := request.URL.Query()
	options := readservice.RunListOptions{
		Gaggle:   query.Get("gaggle"),
		Workflow: query.Get("workflow"),
		Phase:    readservice.RunPhase(query.Get("phase")),
		Trigger:  readservice.TriggerKind(query.Get("trigger")),
		Cursor:   query.Get("cursor"),
	}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return readservice.RunListOptions{}, fmt.Errorf("%w: limit must be an integer", readservice.ErrInvalidArgument)
		}
		options.Limit = limit
	}
	return options, nil
}

func writeReadError(w http.ResponseWriter, errorLog *log.Logger, operation string, err error) {
	switch {
	case errors.Is(err, readservice.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, "invalid_argument", "request parameters are invalid")
	case errors.Is(err, readservice.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "requested run data was not found")
	case errors.Is(err, readservice.ErrArtifactIntegrity):
		errorLog.Printf("%s failed: %v", operation, err)
		writeError(w, http.StatusConflict, "artifact_invalid", "artifact integrity verification failed")
	default:
		errorLog.Printf("%s failed: %v", operation, err)
		writeError(w, http.StatusInternalServerError, "read_error", "runtime state could not be read")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorEnvelope{Error: APIError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
