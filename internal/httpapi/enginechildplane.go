package httpapi

import (
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/engine"
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

// WithEngineChildAdmissions installs the control-side generated-child custody
// owner behind the engine child-admission plane.
func WithEngineChildAdmissions(service engine.ChildAdmissions) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API engine child admissions are required")
		}
		config.engineChildren = service
		return nil
	}
}

func engineChildPlanePath(path string) bool {
	return path == apicontract.EngineChildAdmitPath || path == apicontract.EngineChildLoadPath || path == apicontract.EngineChildCancelPath
}

// engineChildRequest enforces the dedicated worker credential even under a
// permissive authorizer, then the mutation transport and a bounded body.
func engineChildRequest(w http.ResponseWriter, request *http.Request, config handlerConfig, target any) bool {
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
	if err := decodeWriteRequestBounded(request, target, maxEngineChildRequest); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return false
	}
	return true
}

func registerEngineChildPlaneRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	router.Handle(apicontract.RouteEngineChildAdmit, func(w http.ResponseWriter, request *http.Request) {
		var in engine.ChildAdmissionInput
		if !engineChildRequest(w, request, config, &in) {
			return
		}
		binding, err := config.engineChildren.Admit(request.Context(), in)
		if err != nil {
			writePlaneError(w, errorLog, "admit engine child", err)
			return
		}
		writeJSON(w, http.StatusOK, binding)
	})
	router.Handle(apicontract.RouteEngineChildLoad, func(w http.ResponseWriter, request *http.Request) {
		var binding engine.ChildRunBinding
		if !engineChildRequest(w, request, config, &binding) {
			return
		}
		manifest, err := config.engineChildren.Load(request.Context(), binding)
		if err != nil {
			writePlaneError(w, errorLog, "load engine child manifest", err)
			return
		}
		if len(manifest) == 0 || len(manifest) > MaxEngineChildManifestBytes {
			writeError(w, http.StatusInternalServerError, "child_manifest_too_large", "stored manifest exceeds the response budget")
			return
		}
		writeJSON(w, http.StatusOK, EngineChildManifest{Manifest: manifest})
	})
	router.Handle(apicontract.RouteEngineChildCancel, func(w http.ResponseWriter, request *http.Request) {
		var binding engine.ChildRunBinding
		if !engineChildRequest(w, request, config, &binding) {
			return
		}
		if err := config.engineChildren.Cancel(request.Context(), binding); err != nil {
			writePlaneError(w, errorLog, "cancel engine child family", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
