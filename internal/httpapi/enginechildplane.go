package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
)

const (
	// maxEngineChildRequest bounds admission inputs and receipts, which the
	// engine itself bounds to a few KiB.
	maxEngineChildRequest = 64 << 10
	// MaxEngineChildManifestBytes bounds one frozen manifest: the largest
	// retained proposal plus its pins.
	MaxEngineChildManifestBytes = 2 << 20
)

// EngineChildManifest is the load response: the exact digest-addressed bytes.
type EngineChildManifest struct {
	Manifest []byte `json:"manifest"`
}

// EngineChildPlane is the control-side generated-child custody owner behind
// the engine child-admission plane. It is deliberately free of engine types:
// internal/engine imports internal/dispatcher, whose tests import this
// package, so the engine-typed adapter lives in internal/enginechildplane and
// is composed in. Each method decodes its own request exactly once through
// decode, which applies the plane's strict, bounded JSON decoding.
type EngineChildPlane interface {
	Admit(ctx context.Context, decode func(any) error) (any, error)
	Load(ctx context.Context, decode func(any) error) ([]byte, error)
	Cancel(ctx context.Context, decode func(any) error) error
}

// EngineChildAuthority is the control-side owner of engine parents'
// child-workflow grants behind the same plane, engine-type free for the same
// reason as EngineChildPlane. Acquire's result carries a bearer: the response
// is never cached and never logged.
type EngineChildAuthority interface {
	Acquire(ctx context.Context, decode func(any) error) (any, error)
	Release(ctx context.Context, decode func(any) error) error
	Observe(ctx context.Context, decode func(any) error) (any, error)
	Origin(ctx context.Context, worker string, decode func(any) error) (any, error)
}

// EngineChildProtocol is the rest of the engine child protocol behind the
// same plane (#8085 d): Park lends a parked parent's capacity, Finalize seals
// a child's result, Complete resumes the parent with it. Engine-type free for
// the same reason as EngineChildPlane.
type EngineChildProtocol interface {
	Park(ctx context.Context, decode func(any) error) error
	Finalize(ctx context.Context, decode func(any) error) (any, error)
	Complete(ctx context.Context, decode func(any) error) (any, error)
}

// WithEngineChildProtocol installs the parking, finalizing and completing
// owner behind the engine child-admission plane.
func WithEngineChildProtocol(protocol EngineChildProtocol) HandlerOption {
	return func(config *handlerConfig) error {
		if protocol == nil {
			return errors.New("http API engine child protocol is required")
		}
		config.engineChildProtocol = protocol
		return nil
	}
}

// WithEngineChildAuthority installs the owner of engine parents' child-workflow
// grants behind the engine child-admission plane.
func WithEngineChildAuthority(authority EngineChildAuthority) HandlerOption {
	return func(config *handlerConfig) error {
		if authority == nil {
			return errors.New("http API engine child authority is required")
		}
		config.engineChildAuthority = authority
		return nil
	}
}

// WithEngineChildAdmissions installs the control-side generated-child custody
// owner behind the engine child-admission plane.
func WithEngineChildAdmissions(plane EngineChildPlane) HandlerOption {
	return func(config *handlerConfig) error {
		if plane == nil {
			return errors.New("http API engine child admissions are required")
		}
		config.engineChildren = plane
		return nil
	}
}

func engineChildPlanePath(path string) bool {
	return path == apicontract.EngineChildOriginPath || path == apicontract.EngineChildAdmitPath || path == apicontract.EngineChildLoadPath || path == apicontract.EngineChildCancelPath ||
		path == apicontract.EngineChildHandoffPath || path == apicontract.EngineChildAuthorityPath || path == apicontract.EngineChildAuthorityReleasePath ||
		path == apicontract.EngineChildParkPath || path == apicontract.EngineChildFinalizePath || path == apicontract.EngineChildCompletePath
}

// engineChildAuthorized enforces the dedicated worker credential even under a
// permissive authorizer, then the mutation transport. available reports
// whether this server installed the owner behind the route.
func engineChildAuthorized(w http.ResponseWriter, request *http.Request, available bool) bool {
	w.Header().Set("Cache-Control", "private, no-store")
	principal, ok := PrincipalFromRequest(request)
	if !ok || principal.Issuer != WorkerChildAdmissionPrincipalIssuer {
		writeError(w, http.StatusForbidden, "child_admission_worker_required", "engine child admission requires a dedicated worker credential")
		return false
	}
	if !available {
		writeError(w, http.StatusServiceUnavailable, "child_admission_unavailable", "engine child admission is not available from this server")
		return false
	}
	if status, code, message := validateMutationTransport(request); status != 0 {
		writeError(w, status, code, message)
		return false
	}
	return true
}

// engineChildDecoder is the one bounded strict decode a plane call may make.
// A decode failure is the caller's fault (400); anything else the plane
// returns is a custody failure.
type engineChildDecoder struct {
	request *http.Request
	calls   int
	err     error
}

func (d *engineChildDecoder) decode(target any) error {
	d.calls++
	if d.calls > 1 {
		return errors.New("engine child plane decoded its request twice")
	}
	d.err = decodeWriteRequestBounded(d.request, target, maxEngineChildRequest)
	return d.err
}

// handleEngineChild authorizes, runs one plane call and maps its errors;
// respond writes the success response.
func handleEngineChild(available bool, errorLog *log.Logger, op string, call func(context.Context, func(any) error) (any, error), respond func(http.ResponseWriter, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if !engineChildAuthorized(w, request, available) {
			return
		}
		decoder := &engineChildDecoder{request: request}
		out, err := call(request.Context(), decoder.decode)
		switch {
		case decoder.err != nil:
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, decoder.err.Error())
		case decoder.calls != 1:
			writePlaneError(w, errorLog, op, fmt.Errorf("engine child plane decoded its request %d times, want 1", decoder.calls))
		case err != nil:
			writePlaneError(w, errorLog, op, err)
		default:
			respond(w, out)
		}
	}
}

func registerEngineChildPlaneRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	plane := func() EngineChildPlane { return config.engineChildren }
	children := config.engineChildren != nil
	router.Handle(apicontract.RouteEngineChildAdmit, handleEngineChild(children, errorLog, "admit engine child",
		func(ctx context.Context, decode func(any) error) (any, error) { return plane().Admit(ctx, decode) },
		func(w http.ResponseWriter, binding any) { writeJSON(w, http.StatusOK, binding) }))
	router.Handle(apicontract.RouteEngineChildLoad, handleEngineChild(children, errorLog, "load engine child manifest",
		func(ctx context.Context, decode func(any) error) (any, error) { return plane().Load(ctx, decode) },
		func(w http.ResponseWriter, out any) {
			manifest, _ := out.([]byte)
			if len(manifest) == 0 || len(manifest) > MaxEngineChildManifestBytes {
				writeError(w, http.StatusInternalServerError, "child_manifest_too_large", "stored manifest exceeds the response budget")
				return
			}
			writeJSON(w, http.StatusOK, EngineChildManifest{Manifest: manifest})
		}))
	router.Handle(apicontract.RouteEngineChildCancel, handleEngineChild(children, errorLog, "cancel engine child family",
		func(ctx context.Context, decode func(any) error) (any, error) {
			return nil, plane().Cancel(ctx, decode)
		},
		func(w http.ResponseWriter, _ any) { w.WriteHeader(http.StatusNoContent) }))
	authority := config.engineChildAuthority
	granted := authority != nil
	registerEngineChildOrigin(router, authority, errorLog)
	router.Handle(apicontract.RouteEngineChildAuthority, handleEngineChild(granted, errorLog, "acquire engine child authority",
		func(ctx context.Context, decode func(any) error) (any, error) { return authority.Acquire(ctx, decode) },
		func(w http.ResponseWriter, grant any) { writeJSON(w, http.StatusOK, grant) }))
	router.Handle(apicontract.RouteEngineChildHandoff, handleEngineChild(granted, errorLog, "observe engine child handoff",
		func(ctx context.Context, decode func(any) error) (any, error) { return authority.Observe(ctx, decode) },
		func(w http.ResponseWriter, request any) { writeJSON(w, http.StatusOK, request) }))
	router.Handle(apicontract.RouteEngineChildAuthorityRelease, handleEngineChild(granted, errorLog, "release engine child authority",
		func(ctx context.Context, decode func(any) error) (any, error) {
			return nil, authority.Release(ctx, decode)
		},
		func(w http.ResponseWriter, _ any) { w.WriteHeader(http.StatusNoContent) }))
	protocol := config.engineChildProtocol
	served := protocol != nil
	router.Handle(apicontract.RouteEngineChildPark, handleEngineChild(served, errorLog, "park engine child parent",
		func(ctx context.Context, decode func(any) error) (any, error) {
			return nil, protocol.Park(ctx, decode)
		},
		func(w http.ResponseWriter, _ any) { w.WriteHeader(http.StatusNoContent) }))
	router.Handle(apicontract.RouteEngineChildFinalize, handleEngineChild(served, errorLog, "finalize engine child",
		func(ctx context.Context, decode func(any) error) (any, error) { return protocol.Finalize(ctx, decode) },
		func(w http.ResponseWriter, result any) { writeJSON(w, http.StatusOK, result) }))
	router.Handle(apicontract.RouteEngineChildComplete, handleEngineChild(served, errorLog, "complete engine child",
		func(ctx context.Context, decode func(any) error) (any, error) { return protocol.Complete(ctx, decode) },
		func(w http.ResponseWriter, pointer any) { writeJSON(w, http.StatusOK, pointer) }))
}
