package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// TerminalInterventionService is optional even when ordinary emits are served.
// It commits the fenced, keyed HITL prefix before returning a durable receipt.
type TerminalInterventionService interface {
	CommitTerminalIntervention(context.Context, livejournal.TerminalInterventionRequest) (journal.TerminalInterventionReceipt, error)
}

func terminalInterventionPath(path string) bool {
	rest, ok := strings.CutPrefix(path, apicontract.V1Prefix+"/runs/")
	if !ok {
		return false
	}
	run, ok := strings.CutSuffix(rest, "/journal/terminal-intervention")
	return ok && apiv1.ValidRunID(run)
}

func authorizeWorkerTerminalIntervention(request *http.Request) error {
	if request.Method == http.MethodPost && terminalInterventionPath(request.URL.Path) {
		return nil
	}
	return errors.New("terminal-intervention worker may only commit keyed terminal journal transitions")
}

func registerTerminalInterventionRoute(router *Router, config handlerConfig, errorLog *log.Logger) {
	router.Handle(apicontract.RouteJournalTerminalIntervention, func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		principal, ok := PrincipalFromRequest(request)
		if !ok || principal.Issuer != WorkerTerminalInterventionPrincipalIssuer {
			writeError(w, http.StatusForbidden, "terminal_intervention_worker_required", "terminal journal intervention requires a dedicated worker credential")
			return
		}
		service, ok := config.journal.(TerminalInterventionService)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "terminal_intervention_unavailable", "terminal journal intervention is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input livejournal.TerminalInterventionRequest
		if err := decodeWriteRequestBounded(request, &input, maxJournalEmitBody); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		run := request.PathValue("run")
		if !apiv1.ValidRunID(run) || input.RunID != "" && input.RunID != run {
			writeError(w, http.StatusBadRequest, "run_mismatch", "intervention run id must match the route")
			return
		}
		input.RunID = run
		receipt, err := service.CommitTerminalIntervention(request.Context(), input)
		switch {
		case errors.Is(err, journal.ErrInterventionInvalid):
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		case errors.Is(err, journal.ErrInterventionCancelled), errors.Is(err, journal.ErrInterventionStale), errors.Is(err, journal.ErrInterventionConflict), errors.Is(err, journal.ErrInterventionPending):
			writeError(w, http.StatusConflict, "terminal_intervention_conflict", err.Error())
		case err != nil:
			writeJournalPlaneError(w, errorLog, err)
		default:
			w.Header().Set(HeaderSourceApplied, fmt.Sprintf("%s:%d", run, receipt.Seq))
			writeJSON(w, http.StatusOK, receipt)
		}
	})
}
