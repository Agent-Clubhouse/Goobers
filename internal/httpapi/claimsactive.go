package httpapi

import (
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

// registerActiveClaimsRoute serves what this instance has claimed right now
// (#1488). A reader without the surface answers 503 and discovery reports the
// route unconfigured, the same shape as the work-item routes.
func registerActiveClaimsRoute(router *Router, reader readservice.Reader, errorLog *log.Logger) {
	claims, available := reader.(readservice.ActiveClaimsReader)
	router.Handle(apicontract.RouteClaimsActive, func(w http.ResponseWriter, request *http.Request) {
		if !available {
			writeError(w, http.StatusServiceUnavailable, "service_unconfigured",
				"active claims are not served by this API")
			return
		}
		value, err := claims.ActiveClaims(request.Context())
		if err != nil {
			writeInventoryReadError(w, errorLog, "active claims", err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	})
}
