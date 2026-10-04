package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
)

// ChildPublicationCheckService rechecks current human access, records the actor
// and observes one existing immutable intent. It cannot initiate publication.
type ChildPublicationCheckService interface {
	CheckChildPublication(context.Context, Principal, string, string, apicontract.ChildPublicationCheckRequest) (apicontract.ChildPublicationCheckResult, error)
}

// WithChildPublicationChecks installs the authorized provider observation bridge.
func WithChildPublicationChecks(service ChildPublicationCheckService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("child publication check service is required")
		}
		config.childPublicationChecks = service
		return nil
	}
}

func registerChildPublicationCheckRoute(router *Router, config handlerConfig, errorLog *log.Logger) {
	router.Handle(apicontract.RouteChildPublicationCheck, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		principal, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if !principal.HasRole(RoleOperate) {
			writeError(w, http.StatusForbidden, "interactive_access_denied", "Publication observation requires operator access.")
			return
		}
		if err := validateInteractiveTransport(r); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if r.URL.RawQuery != "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Publication checks do not accept query parameters.")
			return
		}
		key, ok := requireIdempotencyKey(w, r)
		if !ok {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var input apicontract.ChildPublicationCheckRequest
		if err := decodeWriteRequestBounded(r, &input, 4097); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Invalid publication check request.")
			return
		}
		if (input.Action != "branch" && input.Action != "pr") || !blobstore.ValidDigest(input.ExpectedIntentDigest) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Select an existing branch or PR publication intent.")
			return
		}
		if config.childPublicationChecks == nil {
			writeError(w, http.StatusServiceUnavailable, "child_publication_check_unavailable", "Publication observation is unavailable on this server.")
			return
		}
		result, err := config.childPublicationChecks.CheckChildPublication(r.Context(), principal, r.PathValue("run"), key, input)
		if err != nil {
			writePlaneError(w, errorLog, "check child publication", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
