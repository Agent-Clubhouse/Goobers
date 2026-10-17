package httpapi

import (
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
	"log"
	"net/http"
)

func registerRunAttributionRoute(router *Router, reader readservice.Reader, errorLog *log.Logger) {
	attribution, available := reader.(readservice.RunAttributionReader)
	router.Handle(apicontract.RouteRunAttribution, func(w http.ResponseWriter, r *http.Request) {
		if !available {
			writeError(w, http.StatusNotImplemented, "attribution_unavailable", "run attribution is unavailable from this reader")
			return
		}
		gaggle := r.URL.Query().Get("gaggle")
		if len(r.URL.Query()["gaggle"]) != 1 {
			writeReadError(w, errorLog, "read attribution", readservice.ErrInvalidArgument)
			return
		}
		result, err := attribution.RunAttribution(r.Context(), r.PathValue("run"), gaggle)
		if err != nil {
			writeReadError(w, errorLog, "read attribution", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
