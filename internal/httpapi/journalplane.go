package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/livejournal"
)

// journalplane.go is the write API's journal plane
// (distributed-state-and-coordination.md §7/§8, DS4): batched live journal
// events for one run, accepted here and appended by the daemon's single
// writer with sequence assigned at acceptance. Span adoption by digest rides
// the same route as a span-kind op — a second endpoint would be a second
// spelling of the same write. Pod principals are confined to their own run
// (the #3524 podauth pattern); the daemon's in-process emitters never pass
// through HTTP at all.

// maxJournalEmitBody bounds one emit batch. Larger than the shared mutation
// cap because artifact ops carry their bytes inline (context manifests, gate
// verdicts) — bounded values by construction, but comfortably above 1 MiB in
// pathological verdicts.
const maxJournalEmitBody = 4 << 20

// JournalService is the daemon-side journal plane. The shipped implementation
// is *livejournal.Writer; the wire types are the livejournal package's own so
// the in-process seam and this transport cannot drift.
type JournalService interface {
	Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error)
}

// WithJournalService enables the journal-plane emit route.
func WithJournalService(service JournalService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API journal service is required")
		}
		config.journal = service
		return nil
	}
}

func registerJournalPlaneRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	journal := config.journal
	router.Handle(apicontract.RouteJournalEmit, func(w http.ResponseWriter, request *http.Request) {
		if journal == nil {
			writeError(w, http.StatusServiceUnavailable, "journal_unavailable", "the journal plane is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input livejournal.EmitRequest
		if err := decodeWriteRequestBounded(request, &input, maxJournalEmitBody); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		run := request.PathValue("run")
		if !apiv1.ValidRunID(run) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "run id is not a safe path segment")
			return
		}
		if input.RunID != "" && input.RunID != run {
			writeError(w, http.StatusBadRequest, "run_mismatch", "request body run id does not match the route")
			return
		}
		input.RunID = run
		if len(input.Ops) == 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "ops are required")
			return
		}
		// Per-run containment: a pod token proves "I am run X's stage pod",
		// which authorizes journal emission for run X and no other — the same
		// body-level binding the claims plane applies.
		if principal, ok := PrincipalFromRequest(request); ok && IsPodPrincipal(principal) {
			if principal.Subject != podPrincipalSubject(run) {
				writeError(w, http.StatusForbidden, "run_mismatch", "pod principal may only emit into its own run's journal")
				return
			}
		}
		response, err := journal.Emit(request.Context(), input)
		if err != nil {
			writeJournalPlaneError(w, errorLog, err)
			return
		}
		if response.Seq > 0 {
			w.Header().Set(HeaderSourceApplied, fmt.Sprintf("%s:%d", run, response.Seq))
		}
		writeJSON(w, http.StatusOK, response)
	})
}

// writeJournalPlaneError maps writer refusals onto typed API errors: a
// terminal journal is a conflict the emitter must stop retrying (the run is
// closed; only the repair projection may touch it now), an unknown run with
// no open header is the caller's error, and everything else is the shared
// write-plane mapping.
func writeJournalPlaneError(w http.ResponseWriter, errorLog *log.Logger, err error) {
	switch {
	case errors.Is(err, livejournal.ErrTerminal):
		writeError(w, http.StatusConflict, "journal_terminal", "the run journal is terminal; new events are refused")
	case errors.Is(err, livejournal.ErrUnknownRun):
		writeError(w, http.StatusBadRequest, "journal_unopened", "the run journal does not exist and the emit carries no open header")
	default:
		writePlaneError(w, errorLog, "emit journal events", err)
	}
}
