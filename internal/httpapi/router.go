// Package httpapi exposes the versioned, read-only loopback HTTP adapter over
// readservice.Reader.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strconv"

	"github.com/goobers/goobers/internal/readservice"
)

const (
	// Prefix is the versioned root for all HTTP API routes.
	Prefix = "/api/v1"
	// HealthPath is the daemon health endpoint.
	HealthPath = Prefix + "/health"
	// TelemetryStatsPath exposes workflow and stage telemetry aggregates.
	TelemetryStatsPath = Prefix + "/telemetry/stats"
	// TelemetryErrorsPath exposes paginated recent telemetry errors.
	TelemetryErrorsPath = Prefix + "/telemetry/errors"
	// RunsPath is the run history endpoint.
	RunsPath = Prefix + "/runs"
	// InstancePath is the instance inventory endpoint.
	InstancePath = Prefix + "/instance"
	// GagglesPath is the gaggle inventory endpoint.
	GagglesPath = Prefix + "/gaggles"
	// GaggleGoobersPath is the gaggle-scoped goober inventory route.
	GaggleGoobersPath = Prefix + "/gaggles/{gaggle}/goobers"
	// GaggleWorkflowsPath is the gaggle-scoped workflow inventory route.
	GaggleWorkflowsPath = Prefix + "/gaggles/{gaggle}/workflows"
	// WorkflowDetailPath is the gaggle-scoped workflow detail route.
	WorkflowDetailPath = Prefix + "/gaggles/{gaggle}/workflows/{workflow}"
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
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	return &Router{mux: mux, authorizer: authorizer}, nil
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
	registerTelemetryRoutes(router, reader, errorLog)
	registerRunRoutes(router, reader, errorLog)
	registerInventoryRoutes(router, reader, errorLog)
	return router.Handler(), nil
}

func registerRunRoutes(router *Router, reader readservice.Reader, errorLog *log.Logger) {
	router.HandleGET(RunsPath, func(w http.ResponseWriter, request *http.Request) {
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
	router.HandleGET(RunsPath+"/{run}", func(w http.ResponseWriter, request *http.Request) {
		run, err := reader.GetRun(request.Context(), request.PathValue("run"))
		if err != nil {
			writeReadError(w, errorLog, "get run", err)
			return
		}
		writeJSON(w, http.StatusOK, run)
	})
	router.HandleGET(RunsPath+"/{run}/events", func(w http.ResponseWriter, request *http.Request) {
		events, err := reader.RunEvents(request.Context(), request.PathValue("run"))
		if err != nil {
			writeReadError(w, errorLog, "read run events", err)
			return
		}
		writeJSON(w, http.StatusOK, events)
	})
	router.HandleGET(RunsPath+"/{run}/stages/{stage}/attempts", func(w http.ResponseWriter, request *http.Request) {
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
	router.HandleGET(RunsPath+"/{run}/artifacts/{digest}", func(w http.ResponseWriter, request *http.Request) {
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
