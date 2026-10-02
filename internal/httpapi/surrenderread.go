package httpapi

import (
	"context"
	"log"
	"net/http"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// SurrenderReader is optional for write-only backends. Reads must enforce the
// budget before allocating a stored document, not after loading an entire file.
type SurrenderReader interface {
	Has(context.Context, string, string, int) (bool, error)
	GetBounded(context.Context, string, string, int, int64) ([]byte, error)
}

func surrenderReadHandler(plane SurrenderService, seenOnly bool, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		// Also enforce the identity when a caller configures a permissive authorizer.
		principal, ok := PrincipalFromRequest(request)
		if !ok || principal.Issuer != WorkerSurrenderPrincipalIssuer {
			writeError(w, http.StatusForbidden, "surrender_worker_required", "surrender reads require a dedicated worker credential")
			return
		}
		reader, ok := plane.(SurrenderReader)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "surrender_unavailable", "surrender reads are not available from this server")
			return
		}
		run, stage := request.PathValue("run"), request.PathValue("stage")
		attempt, err := strconv.Atoi(request.PathValue("attempt"))
		if !apiv1.ValidRunID(run) || !apiv1.ValidRunID(stage) || err != nil || attempt < 1 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "surrender identity requires safe run, stage, and positive attempt")
			return
		}
		seen, err := reader.Has(request.Context(), run, stage, attempt)
		if err != nil {
			writePlaneError(w, errorLog, "read surrender presence", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if seenOnly {
			writeJSON(w, http.StatusOK, struct {
				Seen bool `json:"seen"`
			}{seen})
			return
		}
		if !seen {
			writeError(w, http.StatusNotFound, "no_surrender", "no result has been surrendered")
			return
		}
		data, err := reader.GetBounded(request.Context(), run, stage, attempt, maxSurrenderBody)
		if err != nil {
			writePlaneError(w, errorLog, "read surrendered result", err)
			return
		}
		if len(data) > maxSurrenderBody {
			writeError(w, http.StatusInternalServerError, "surrender_too_large", "stored surrender exceeds response budget")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}
