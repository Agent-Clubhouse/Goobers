package httpapi

import (
	"context"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
)

// ChildWorkflowAccessResponse is a short-lived secret delivery, never a journal
// or execution-kit artifact. The host binds it to the signed parent contract.
type ChildWorkflowAccessResponse struct {
	Endpoint    string `json:"endpoint"`
	BearerToken string `json:"bearerToken"`
}

// ChildWorkflowAccessService is an optional extension of the credential plane.
// Implementations revalidate signed contract custody and live stage authority;
// request bodies supply no actor, policy or origin authority.
type ChildWorkflowAccessService interface {
	AcquireChildWorkflowAccess(context.Context, string, string) (ChildWorkflowAccessResponse, error)
	RevokeChildWorkflowAccess(context.Context, string, string) error
}

func registerParentAccessRoutes(router *Router, credentials CredentialService, logger *log.Logger) {
	service, _ := credentials.(ChildWorkflowAccessService)
	router.HandleByMethod(map[string]apicontract.RouteID{http.MethodPost: apicontract.RouteChildWorkflowAccessAcquire, http.MethodDelete: apicontract.RouteChildWorkflowAccessRevoke}, map[apicontract.RouteID]http.HandlerFunc{
		apicontract.RouteChildWorkflowAccessAcquire: parentAccessHandler(service, false, logger),
		apicontract.RouteChildWorkflowAccessRevoke:  parentAccessHandler(service, true, logger),
	})
}

func parentAccessHandler(service ChildWorkflowAccessService, revoke bool, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := PrincipalFromRequest(r)
		run := r.PathValue("run")
		if !ok || !IsPodPrincipal(p) || !p.WorkflowParent || p.GeneratedChild || p.Subject != "run:"+run || !blobstore.ValidDigest(p.WorkflowParentContractDigest) {
			writeError(w, http.StatusForbidden, "parent_pod_required", "an exact contained parent attempt is required")
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "parent_access_unavailable", "parent access is unavailable")
			return
		}
		if status, code, message := validateMutationTransport(r); status != 0 {
			writeError(w, status, code, message)
			return
		}
		digest, err := childStringBody(r, "contractDigest", 71)
		if err != nil || digest != p.WorkflowParentContractDigest {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "contractDigest must match the signed parent attempt")
			return
		}
		if revoke {
			if err := service.RevokeChildWorkflowAccess(r.Context(), run, digest); err != nil {
				writePlaneError(w, logger, "revoke parent access", err)
				return
			}
			writeJSON(w, http.StatusOK, struct{}{})
			return
		}
		access, err := service.AcquireChildWorkflowAccess(r.Context(), run, digest)
		if err != nil {
			writePlaneError(w, logger, "acquire parent access", err)
			return
		}
		writeJSON(w, http.StatusOK, access)
	}
}
