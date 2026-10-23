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
	return path == apicontract.EngineChildAdmitPath || path == apicontract.EngineChildLoadPath || path == apicontract.EngineChildCancelPath
}

// engineChildAuthorized enforces the dedicated worker credential even under a
// permissive authorizer, then the mutation transport.
func engineChildAuthorized(w http.ResponseWriter, request *http.Request, config handlerConfig) bool {
	w.Header().Set("Cache-Control", "private, no-store")
	principal, ok := PrincipalFromRequest(request)
	if !ok || principal.Issuer != WorkerChildAdmissionPrincipalIssuer {
		writeError(w, http.StatusForbidden, "child_admission_worker_required", "engine child admission requires a dedicated worker credential")
		return false
	}
	if config.engineChildren == nil {
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
func handleEngineChild(config handlerConfig, errorLog *log.Logger, op string, call func(context.Context, func(any) error) (any, error), respond func(http.ResponseWriter, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if !engineChildAuthorized(w, request, config) {
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
	router.Handle(apicontract.RouteEngineChildAdmit, handleEngineChild(config, errorLog, "admit engine child",
		func(ctx context.Context, decode func(any) error) (any, error) { return plane().Admit(ctx, decode) },
		func(w http.ResponseWriter, binding any) { writeJSON(w, http.StatusOK, binding) }))
	router.Handle(apicontract.RouteEngineChildLoad, handleEngineChild(config, errorLog, "load engine child manifest",
		func(ctx context.Context, decode func(any) error) (any, error) { return plane().Load(ctx, decode) },
		func(w http.ResponseWriter, out any) {
			manifest, _ := out.([]byte)
			if len(manifest) == 0 || len(manifest) > MaxEngineChildManifestBytes {
				writeError(w, http.StatusInternalServerError, "child_manifest_too_large", "stored manifest exceeds the response budget")
				return
			}
			writeJSON(w, http.StatusOK, EngineChildManifest{Manifest: manifest})
		}))
	router.Handle(apicontract.RouteEngineChildCancel, handleEngineChild(config, errorLog, "cancel engine child family",
		func(ctx context.Context, decode func(any) error) (any, error) {
			return nil, plane().Cancel(ctx, decode)
		},
		func(w http.ResponseWriter, _ any) { w.WriteHeader(http.StatusNoContent) }))
}
