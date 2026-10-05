package httpapi

import (
	"context"
	"errors"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
)

// InteractiveRunService requires explicit human gaggle authority and uses a
// retained run to resolve scope. Request payloads cannot supply authority.
type InteractiveRunService interface {
	InspectInteractiveRun(context.Context, Principal, string) (apicontract.InteractiveRunView, error)
	AcceptInteractiveRun(context.Context, context.Context, Principal, string, string, apicontract.InteractiveRunCommand) (apicontract.InteractiveRunCommandResult, error)
}

// WithInteractiveRuns enables the shared human gate and guidance surface.
func WithInteractiveRuns(service InteractiveRunService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("interactive run service is required")
		}
		config.interactiveRuns = service
		return nil
	}
}

func interactiveHuman(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, ok := PrincipalFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "interactive_authentication_required", "interactive access requires an authenticated human")
		return p, false
	}
	if p.Subject == "" || p.Issuer == "" || strings.HasPrefix(p.Issuer, "goobers/") || !p.HasRole(RoleView) {
		writeError(w, http.StatusForbidden, "interactive_access_denied", "interactive access is not authorized")
		return p, false
	}
	return p, true
}

func registerInteractiveRunRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	router.Handle(apicontract.RouteInteractiveRun, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if config.interactiveRuns == nil {
			writeError(w, http.StatusServiceUnavailable, "interactive_unavailable", "interactive run operations are unavailable")
			return
		}
		result, err := config.interactiveRuns.InspectInteractiveRun(r.Context(), p, r.PathValue("run"))
		if err != nil {
			writePlaneError(w, errorLog, "inspect interactive run", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	router.Handle(apicontract.RouteInteractiveRunCommand, interactiveRunCommandHandler(config, errorLog))
}

func interactiveRunCommandHandler(config handlerConfig, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if !p.HasRole(RoleOperate) {
			writeError(w, http.StatusForbidden, "interactive_access_denied", "interactive operation is not authorized")
			return
		}
		if config.interactiveRuns == nil {
			writeError(w, http.StatusServiceUnavailable, "interactive_unavailable", "interactive run operations are unavailable")
			return
		}
		if err := validateInteractiveTransport(r); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		key, ok := requireIdempotencyKey(w, r)
		if !ok {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 400<<10) // permits JSON escaping of bounded 64 KiB text.
		var input apicontract.InteractiveRunCommand
		if err := decodeWriteRequestBounded(r, &input, (400<<10)+1); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		result, err := config.interactiveRuns.AcceptInteractiveRun(r.Context(), config.interventionContext, p, r.PathValue("run"), key, input)
		if err != nil {
			writePlaneError(w, errorLog, "interactive run command", err)
			return
		}
		status := http.StatusOK
		if result.Status == "pending" {
			status = http.StatusAccepted
		}
		writeJSON(w, status, result)
	}
}

func validateInteractiveTransport(r *http.Request) error {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.EqualFold(parsed.Host, r.Host) {
		return errors.New("cross-origin mutation requests are forbidden")
	}
	return nil
}
