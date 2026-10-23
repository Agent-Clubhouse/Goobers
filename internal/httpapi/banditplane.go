package httpapi

import (
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/bandit"
)

// WithBanditService enables the closed experiment observation plane.
func WithBanditService(service bandit.ObservationPlane) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API bandit service is required")
		}
		config.bandit = service
		return nil
	}
}

func registerBanditPlaneRoutes(router *Router, config handlerConfig) {
	router.Handle(apicontract.RouteBanditObservations, func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input bandit.ObservationRequest
		if err := decodeWriteRequestBounded(request, &input, 64<<10); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if err := input.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if principal, authenticated := PrincipalFromRequest(request); authenticated && IsPodPrincipal(principal) {
			if !requirePodRun(w, principal, input.RunID, "pod principal may only observe as its own run") {
				return
			}
		}
		if config.bandit == nil {
			writeError(w, http.StatusServiceUnavailable, "bandit_unavailable", "the experiment observation plane is unavailable")
			return
		}
		response, err := config.bandit.Observe(request.Context(), input)
		if errors.Is(err, bandit.ErrObservationConflict) {
			writeError(w, http.StatusConflict, "bandit_conflict", "the attempt already has a different outcome")
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "bandit_unavailable", "the experiment observation store is unavailable")
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
}
