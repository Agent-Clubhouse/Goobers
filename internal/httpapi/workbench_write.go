package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workbench"
)

// WorkbenchWriteService must reauthorize exact current actor/source/field before
// acceptance, replay and receipt access. No HTTP request can supply those grants.
type WorkbenchWriteService interface {
	Capabilities(context.Context, Principal, string, string) (workbench.BacklogWriteCapabilities, error)
	Patch(context.Context, Principal, string, string, string, workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error)
	Command(context.Context, Principal, string, string, string) (workbench.BacklogEditCommand, error)
}

// WithWorkbenchWrites installs durable one-attempt native backlog editing.
func WithWorkbenchWrites(service WorkbenchWriteService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("workbench write service is required")
		}
		config.workbenchWrites = service
		return nil
	}
}
func registerWorkbenchWriteRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	for _, id := range []apicontract.RouteID{apicontract.RouteWorkbenchWriteCapabilities, apicontract.RouteWorkbenchCommand} {
		router.Handle(id, workbenchWriteHandler(id, config.workbenchWrites, errorLog))
	}
}
func workbenchWriteHandler(id apicontract.RouteID, service WorkbenchWriteService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		principal, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if !principal.HasRole(RoleOperate) || principal.ChildWorkflow != nil || principal.GeneratedChild || principal.WorkflowParent || len(principal.Scopes) != 0 {
			writeError(w, http.StatusForbidden, "interactive_access_denied", "Backlog commands require a human operator.")
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "workbench_write_unavailable", "Backlog editing is unavailable on this server.")
			return
		}
		result, err := callWorkbenchWrite(w, r, principal, service, id)
		if err != nil {
			writePlaneError(w, errorLog, "workbench write", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}
func callWorkbenchWrite(w http.ResponseWriter, r *http.Request, p Principal, service WorkbenchWriteService, id apicontract.RouteID) (any, error) {
	if r.URL.RawQuery != "" || r.PathValue("source") == "" || len(r.PathValue("source")) > 64 {
		return nil, sessionBadRequest("Invalid workbench source or query.")
	}
	gaggle, source := r.PathValue("gaggle"), r.PathValue("source")
	switch id {
	case apicontract.RouteWorkbenchWriteCapabilities:
		return service.Capabilities(r.Context(), p, gaggle, source)
	case apicontract.RouteWorkbenchCommand:
		command := r.PathValue("command")
		if !validWorkbenchCommandID(command) {
			return nil, sessionBadRequest("Invalid command receipt identity.")
		}
		return service.Command(r.Context(), p, gaggle, source, command)
	default:
		key, err := workbenchWriteKey(r)
		if err != nil {
			return nil, err
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxWorkbenchPatchBytes)
		request, err := decodeWorkbenchPatch(r)
		if err != nil {
			return nil, err
		}
		return service.Patch(r.Context(), p, gaggle, source, key, request)
	}
}
func validWorkbenchCommandID(id string) bool {
	if !strings.HasPrefix(id, "workbench-") || len(id) != 42 {
		return false
	}
	for _, ch := range id[10:] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
func workbenchWriteKey(r *http.Request) (string, error) {
	if err := validateInteractiveTransport(r); err != nil {
		return "", sessionBadRequest("Use application/json and a same-origin request.")
	}
	values := r.Header.Values(HeaderIdempotencyKey)
	key, err := idempotencyKey(r)
	if err != nil || len(values) != 1 || values[0] != key {
		return "", sessionBadRequest("One exact Idempotency-Key of 1–200 bytes is required.")
	}
	return key, nil
}
