package httpapi

import (
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
)

func registerGaggleHealthRoute(router *Router, service GaggleHealthService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteGaggleHealth, func(w http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "health_unavailable", "gaggle health is unavailable")
			return
		}
		gaggle := request.PathValue("gaggle")
		if !validIdentifier(gaggle) {
			writeError(w, http.StatusBadRequest, "invalid_identifier", "gaggle identifier is invalid")
			return
		}
		response, err := service.Health(request.Context(), gaggle)
		if err != nil {
			errorLog.Printf("gaggle health read failed: %v", err)
			writeError(w, http.StatusInternalServerError, "read_error", "gaggle health could not be read")
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
}
