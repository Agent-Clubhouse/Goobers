package httpapi

import (
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
)

// This route grants no general journal permission. Its owner checks the exact
// outstanding EmitJournal activity loan and scheduled batch before writing.
func registerEngineChildOrigin(router *Router, authority EngineChildAuthority, errorLog *log.Logger) {
	router.Handle(apicontract.RouteEngineChildOrigin, func(w http.ResponseWriter, r *http.Request) {
		if !engineChildAuthorized(w, r, authority != nil) {
			return
		}
		principal, _ := PrincipalFromRequest(r)
		var decodeErr error
		response, err := authority.Origin(r.Context(), principal.Subject, func(target any) error {
			decodeErr = decodeWriteRequestBounded(r, target, maxJournalEmitBody)
			return decodeErr
		})
		if decodeErr != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, decodeErr.Error())
			return
		}
		if err != nil {
			writePlaneError(w, errorLog, "bind engine child origin", err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
}
