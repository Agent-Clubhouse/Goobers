package httpapi

import (
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

// registerRunAttributionRoute serves a run's persisted Backprop record, which
// restates the run's private content (claim statements, failures), under the
// same requester/admin policy as its artifacts and transcripts.
func registerRunAttributionRoute(router *Router, reader readservice.Reader, privacy runPrivacy, errorLog *log.Logger) {
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
		run := r.PathValue("run")
		result, err := attribution.RunAttribution(r.Context(), run, gaggle)
		if err != nil {
			writeReadError(w, errorLog, "read attribution", err)
			return
		}
		privacy.private(w)
		if !privacy.allowed(r, run, "") {
			writeRunContentPrivate(w)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
