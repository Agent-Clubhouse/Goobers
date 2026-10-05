package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workbench"
)

// WorkbenchSuggestionService binds provenance and all source policy server-side.
type WorkbenchSuggestionService interface {
	Artifacts(context.Context, Principal, string, string, uint64) (workbench.SuggestionInventory, error)
	Load(context.Context, Principal, string, workbench.SuggestionSelection) (workbench.SuggestionBatch, error)
	Preview(context.Context, Principal, string, workbench.SuggestionPreviewRequest) (workbench.SuggestionPreview, error)
	Decide(context.Context, Principal, string, workbench.SuggestionDecisionRequest) (workbench.SuggestionReview, error)
	Review(context.Context, Principal, string, string) (workbench.SuggestionReview, error)
}

// WithWorkbenchSuggestions installs explicit human artifact review.
func WithWorkbenchSuggestions(service WorkbenchSuggestionService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("workbench suggestion service is required")
		}
		config.workbenchSuggestions = service
		return nil
	}
}
func registerWorkbenchSuggestionRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	for _, id := range []apicontract.RouteID{apicontract.RouteWorkbenchSuggestionArtifacts, apicontract.RouteWorkbenchSuggestionLoad, apicontract.RouteWorkbenchSuggestionPreview, apicontract.RouteWorkbenchSuggestionDecide, apicontract.RouteWorkbenchSuggestionReview} {
		router.Handle(id, workbenchSuggestionHandler(id, config.workbenchSuggestions, errorLog))
	}
}
func workbenchSuggestionHandler(id apicontract.RouteID, service WorkbenchSuggestionService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
			writeError(w, 403, "interactive_access_denied", "Suggestion review requires a human identity.")
			return
		}
		if (id == apicontract.RouteWorkbenchSuggestionDecide || id == apicontract.RouteWorkbenchSuggestionPreview) && !p.HasRole(RoleOperate) {
			writeError(w, 403, "interactive_access_denied", "Suggestion decisions require a human operator.")
			return
		}
		if service == nil {
			writeError(w, 503, "workbench_suggestions_unavailable", "Suggestion review is unavailable on this server.")
			return
		}
		value, err := callWorkbenchSuggestion(r, p, service, id)
		if err != nil {
			writePlaneError(w, errorLog, "workbench suggestion", err)
			return
		}
		limit := 1 << 20
		if id == apicontract.RouteWorkbenchSuggestionPreview {
			limit = workbench.MaxMetadataPreviewBytes + 4096
		}
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded)+1 > limit {
			writeError(w, 502, "workbench_suggestion_response_invalid", "The suggestion response exceeded its bounded contract.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(append(encoded, '\n'))
	}
}
func callWorkbenchSuggestion(r *http.Request, p Principal, service WorkbenchSuggestionService, id apicontract.RouteID) (any, error) {
	gaggle := r.PathValue("gaggle")
	if id == apicontract.RouteWorkbenchSuggestionArtifacts {
		return suggestionArtifactRequest(r, p, service, gaggle)
	}
	if r.URL.RawQuery != "" {
		return nil, sessionBadRequest("Unexpected suggestion query.")
	}
	if id == apicontract.RouteWorkbenchSuggestionLoad {
		sequence, err := strconv.ParseUint(r.PathValue("sequence"), 10, 53)
		if err != nil || sequence == 0 || strconv.FormatUint(sequence, 10) != r.PathValue("sequence") || !apiv1.ValidRunID(r.PathValue("run")) {
			return nil, sessionBadRequest("Invalid artifact selection.")
		}
		return service.Load(r.Context(), p, gaggle, workbench.SuggestionSelection{RunID: r.PathValue("run"), Sequence: sequence})
	}
	if id == apicontract.RouteWorkbenchSuggestionReview {
		if !validWorkbenchCommandID(r.PathValue("review")) {
			return nil, sessionBadRequest("Invalid review identity.")
		}
		return service.Review(r.Context(), p, gaggle, r.PathValue("review"))
	}
	input, err := decodeSuggestionRequest(r, id)
	if err != nil {
		return nil, err
	}
	switch id {
	case apicontract.RouteWorkbenchSuggestionPreview:
		return service.Preview(r.Context(), p, gaggle, workbench.SuggestionPreviewRequest{Selection: input.Selection, Key: input.Key})
	default:
		return service.Decide(r.Context(), p, gaggle, input)
	}
}
func suggestionArtifactRequest(r *http.Request, p Principal, service WorkbenchSuggestionService, gaggle string) (any, error) {
	run := r.PathValue("run")
	if !apiv1.ValidRunID(run) {
		return nil, sessionBadRequest("Invalid suggestion run identity.")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, sessionBadRequest("Invalid artifact continuation.")
	}
	if len(query) > 1 || (len(query) == 1 && len(query["after"]) != 1) {
		return nil, sessionBadRequest("Only one bounded artifact continuation is supported.")
	}
	var after uint64
	if raw := query.Get("after"); raw != "" {
		after, err = strconv.ParseUint(raw, 10, 53)
		if err != nil || strconv.FormatUint(after, 10) != raw {
			return nil, sessionBadRequest("Invalid artifact continuation.")
		}
	}
	return service.Artifacts(r.Context(), p, gaggle, run, after)
}
func decodeSuggestionRequest(r *http.Request, id apicontract.RouteID) (workbench.SuggestionDecisionRequest, error) {
	var result workbench.SuggestionDecisionRequest
	invalid := sessionBadRequest("Provide one bounded suggestion selection and exact preview for acceptance.")
	if validateInteractiveTransport(r) != nil {
		return result, invalid
	}
	defer func() { _ = r.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16385))
	if err != nil || len(raw) > 16384 || !utf8.Valid(raw) {
		return result, invalid
	}
	allowed, required := []string{"selection", "key"}, []string{"selection", "key"}
	if id == apicontract.RouteWorkbenchSuggestionDecide {
		allowed = append(allowed, "decision", "reason", "expectedOwner", "expectedOperationDigest")
		required = append(required, "decision")
	}
	fields, ok := metadataJSONObject(raw, allowed, required)
	if !ok || !closedSuggestionSelection(fields["selection"]) {
		return result, invalid
	}
	if expected, ok := fields["expectedOwner"]; ok && !closedMetadataExpected(expected) {
		return result, invalid
	}
	if json.Unmarshal(raw, &result) != nil || !suggestionDigest(result.Key) {
		return result, invalid
	}
	if id == apicontract.RouteWorkbenchSuggestionDecide && !validSuggestionDecisionShape(result) {
		return result, invalid
	}
	if !apiv1.ValidRunID(result.Selection.RunID) || result.Selection.Sequence == 0 || result.Selection.Sequence > 9007199254740991 {
		return result, invalid
	}
	return result, nil
}
func closedSuggestionSelection(raw []byte) bool {
	_, ok := metadataJSONObject(raw, []string{"runId", "sequence"}, []string{"runId", "sequence"})
	return ok
}
func suggestionDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}
func validSuggestionDecisionShape(r workbench.SuggestionDecisionRequest) bool {
	if len(r.Reason) > 4096 {
		return false
	}
	if r.Decision == "reject" {
		return r.ExpectedOwner == nil && r.ExpectedOperationDigest == ""
	}
	if r.Decision != "accept" || r.ExpectedOwner == nil || !suggestionDigest(r.ExpectedOperationDigest) {
		return false
	}
	v := r.ExpectedOwner
	return len(v.Commit) == 40 && strings.Trim(v.Commit, "0123456789abcdef") == "" && len(v.BlobID) == 40 && strings.Trim(v.BlobID, "0123456789abcdef") == "" && suggestionDigest(v.ContentDigest)
}
