package httpapi

import (
	"context"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
)

// TriggerStatusRequest is stamped from the authenticated caller, never JSON.
type TriggerStatusRequest struct {
	AcceptanceID string
	Actor        string
	PodScoped    bool
	PodRunID     string
}

// TriggerStatusResponse distinguishes acceptance from a scheduler outcome.
type TriggerStatusResponse = apicontract.TriggerStatusResponse

// TriggerStatusService is implemented by a durable trigger plane.
type TriggerStatusService interface {
	TriggerStatus(context.Context, TriggerStatusRequest) (TriggerStatusResponse, error)
}

func registerTriggerStatusRoute(router *Router, triggers TriggerService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteTriggerStatus, func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		service, ok := triggers.(TriggerStatusService)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "trigger_status_unavailable", "trigger status is not available from this server")
			return
		}
		input := TriggerStatusRequest{AcceptanceID: request.PathValue("acceptance")}
		if principal, ok := PrincipalFromRequest(request); ok {
			input.Actor = principal.Subject
			input.PodScoped = IsPodPrincipal(principal)
			if input.PodScoped {
				var named bool
				input.PodRunID, named = podPrincipalRunID(principal)
				if !named {
					writeError(w, http.StatusForbidden, "run_mismatch", "pod principal does not name a run")
					return
				}
			}
		}
		response, err := service.TriggerStatus(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, "read trigger status", err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
}
