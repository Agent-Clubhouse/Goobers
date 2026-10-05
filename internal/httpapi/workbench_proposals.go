package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workbench"
)

// WorkbenchProposalService must reauthorize every actor, exact configured source,
// credential binding and phase. Neither HTTP preview nor a receipt grants writes.
type WorkbenchProposalService interface {
	Preview(context.Context, Principal, string, string, workbench.MetadataChangeRequest) (workbench.MetadataPreview, error)
	Submit(context.Context, Principal, string, string, string, workbench.MetadataChangeRequest) (workbench.MetadataProposalCommand, error)
	Continue(context.Context, Principal, string, string, string) (workbench.MetadataProposalCommand, error)
	Command(context.Context, Principal, string, string, string) (workbench.MetadataProposalCommand, error)
	Check(context.Context, Principal, string, string, string) (workbench.MetadataProposalCommand, error)
}

// WithWorkbenchProposals installs current-authority, PR-only source editing.
func WithWorkbenchProposals(service WorkbenchProposalService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("workbench proposal service is required")
		}
		config.workbenchProposals = service
		return nil
	}
}
func registerWorkbenchProposalRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	for _, id := range []apicontract.RouteID{apicontract.RouteWorkbenchProposalPreview, apicontract.RouteWorkbenchProposalSubmit, apicontract.RouteWorkbenchProposal, apicontract.RouteWorkbenchProposalCheck, apicontract.RouteWorkbenchProposalContinue} {
		router.Handle(id, workbenchProposalHandler(id, config.workbenchProposals, errorLog))
	}
}
func workbenchProposalHandler(id apicontract.RouteID, service WorkbenchProposalService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
			writeError(w, 403, "interactive_access_denied", "Source proposals require a human identity.")
			return
		}
		if proposalWriteRoute(id) && !p.HasRole(RoleOperate) {
			writeError(w, 403, "interactive_access_denied", "Source proposals require a human operator.")
			return
		}
		if service == nil {
			writeError(w, 503, "workbench_proposals_unavailable", "Source proposals are unavailable on this server.")
			return
		}
		value, err := callWorkbenchProposal(w, r, p, service, id)
		if err != nil {
			writePlaneError(w, errorLog, "workbench proposal", err)
			return
		}
		limit := 128 << 10
		if id == apicontract.RouteWorkbenchProposalPreview {
			limit = workbench.MaxMetadataPreviewBytes
		}
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded)+1 > limit {
			writeError(w, 502, "workbench_proposal_response_invalid", "The proposal response exceeded its bounded contract.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(append(encoded, '\n'))
	}
}
func proposalWriteRoute(id apicontract.RouteID) bool {
	return id == apicontract.RouteWorkbenchProposalPreview || id == apicontract.RouteWorkbenchProposalSubmit || id == apicontract.RouteWorkbenchProposalContinue
}
func callWorkbenchProposal(w http.ResponseWriter, r *http.Request, p Principal, service WorkbenchProposalService, id apicontract.RouteID) (any, error) {
	gaggle, source := r.PathValue("gaggle"), r.PathValue("source")
	if r.URL.RawQuery != "" || source == "" || len(source) > 64 {
		return nil, sessionBadRequest("Invalid workbench source or query.")
	}
	if id == apicontract.RouteWorkbenchProposalPreview || id == apicontract.RouteWorkbenchProposalSubmit {
		if err := validateInteractiveTransport(r); err != nil {
			return nil, sessionBadRequest("Use application/json and a same-origin request.")
		}
		key := ""
		if id == apicontract.RouteWorkbenchProposalSubmit {
			var err error
			key, err = workbenchWriteKey(r)
			if err != nil {
				return nil, err
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxMetadataRequestBytes)
		request, err := decodeMetadataChange(r)
		if err != nil {
			return nil, err
		}
		if id == apicontract.RouteWorkbenchProposalPreview {
			return service.Preview(r.Context(), p, gaggle, source, request)
		}
		return service.Submit(r.Context(), p, gaggle, source, key, request)
	}
	command := r.PathValue("command")
	if !validWorkbenchCommandID(command) {
		return nil, sessionBadRequest("Invalid proposal identity.")
	}
	if id == apicontract.RouteWorkbenchProposal {
		return service.Command(r.Context(), p, gaggle, source, command)
	}
	if err := emptyMetadataCommand(r); err != nil {
		return nil, err
	}
	if id == apicontract.RouteWorkbenchProposalCheck {
		return service.Check(r.Context(), p, gaggle, source, command)
	}
	return service.Continue(r.Context(), p, gaggle, source, command)
}
