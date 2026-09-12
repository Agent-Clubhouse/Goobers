package httpapi

import (
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

func registerQueueEligibilityRoute(router *Router, reader readservice.Reader, errorLog *log.Logger) {
	router.Handle(apicontract.RouteWorkflowQueueEligibility, func(w http.ResponseWriter, request *http.Request) {
		gaggle, workflow := request.PathValue("gaggle"), request.PathValue("workflow")
		if !validIdentifier(gaggle) || !validIdentifier(workflow) {
			writeError(w, http.StatusBadRequest, "invalid_identifier", "gaggle or workflow identifier is invalid")
			return
		}
		value, err := reader.QueueEligibility(request.Context(), gaggle, workflow)
		if err != nil {
			writeInventoryReadError(w, errorLog, "queue eligibility", err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	})
}
