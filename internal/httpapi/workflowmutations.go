package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
)

const maxWorkflowMutationBody = 1 << 16

// WorkflowEnabledRequest sets whether a workflow's non-manual triggers may
// fire. Gaggle/Workflow come from the route path.
type WorkflowEnabledRequest struct {
	Gaggle   string `json:"-"`
	Workflow string `json:"-"`
	Enabled  bool   `json:"enabled"`
}

// WorkflowEnabledResult reports the applied state after a successful toggle.
type WorkflowEnabledResult struct {
	Gaggle   string `json:"gaggle"`
	Workflow string `json:"workflow"`
	Enabled  bool   `json:"enabled"`
}

// WorkflowMutationService edits a workflow's on-disk config and hot-reloads
// the daemon's live definitions from it. Unlike InterventionService (which
// acts on a running run), this rewrites config a running daemon was NOT
// necessarily started to watch, so an implementation must itself trigger a
// reload after writing.
type WorkflowMutationService interface {
	SetWorkflowEnabled(ctx context.Context, input WorkflowEnabledRequest) (WorkflowEnabledResult, error)
}

func registerWorkflowMutationRoutes(router *Router, service WorkflowMutationService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteWorkflowEnabled, workflowEnabledHandler(service, errorLog))
}

func workflowEnabledHandler(service WorkflowMutationService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "workflow_mutations_unavailable", "workflow config mutations are not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		input, err := decodeWorkflowEnabledRequest(request)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		result, err := service.SetWorkflowEnabled(request.Context(), input)
		if err != nil {
			var interventionErr *InterventionError
			if errors.As(err, &interventionErr) {
				status := interventionErr.Status
				if status < 400 || status > 599 {
					status = http.StatusInternalServerError
				}
				if status >= http.StatusInternalServerError {
					errorLog.Printf("set workflow enabled failed: %v", err)
				}
				code := interventionErr.Code
				if code == "" {
					code = "workflow_mutation_failed"
				}
				message := interventionErr.Message
				if message == "" {
					message = "workflow config mutation failed"
				}
				writeError(w, status, code, message)
				return
			}
			errorLog.Printf("set workflow enabled failed: %v", err)
			writeError(w, http.StatusInternalServerError, "workflow_mutation_failed", "workflow config mutation failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func decodeWorkflowEnabledRequest(request *http.Request) (WorkflowEnabledRequest, error) {
	defer func() { _ = request.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxWorkflowMutationBody))
	decoder.DisallowUnknownFields()
	var input WorkflowEnabledRequest
	if err := decoder.Decode(&input); err != nil {
		if errors.Is(err, io.EOF) {
			return WorkflowEnabledRequest{}, errors.New("JSON request body is required")
		}
		return WorkflowEnabledRequest{}, fmt.Errorf("invalid JSON request body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return WorkflowEnabledRequest{}, errors.New("request body must contain one JSON object")
		}
		return WorkflowEnabledRequest{}, fmt.Errorf("invalid JSON request body: %w", err)
	}
	input.Gaggle = request.PathValue("gaggle")
	input.Workflow = request.PathValue("workflow")
	return input, nil
}
