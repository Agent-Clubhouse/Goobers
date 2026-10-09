package httpapi

import (
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

func registerChildHistoryRoute(router *Router, reader readservice.Reader, errorLog *log.Logger) {
	router.Handle(apicontract.RouteRunChildren, func(w http.ResponseWriter, request *http.Request) {
		children, err := reader.RunChildren(request.Context(), request.PathValue("run"), request.URL.Query().Get("cursor"))
		if errors.Is(err, readservice.ErrInvalidCursor) {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "child history cursor is invalid")
			return
		}
		if err != nil {
			writeReadError(w, errorLog, "read child history", err)
			return
		}
		writeJSON(w, http.StatusOK, children)
	})
}
