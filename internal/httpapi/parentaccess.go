package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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

// WithWorkflowParentAccessService installs a dedicated parent exchange owner.
func WithWorkflowParentAccessService(service ChildWorkflowAccessService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("workflow parent access owner is required")
		}
		config.workflowParentAccess = service
		return nil
	}
}

func registerParentAccessRoutes(router *Router, service ChildWorkflowAccessService, logger *log.Logger) {
	router.HandleByMethod(map[string]apicontract.RouteID{http.MethodPost: apicontract.RouteChildWorkflowAccessAcquire, http.MethodDelete: apicontract.RouteChildWorkflowAccessRevoke}, map[apicontract.RouteID]http.HandlerFunc{
		apicontract.RouteChildWorkflowAccessAcquire: parentAccessHandler(service, false, logger),
		apicontract.RouteChildWorkflowAccessRevoke:  parentAccessHandler(service, true, logger),
	})
}

func parentAccessHandler(service ChildWorkflowAccessService, revoke bool, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		p, ok := PrincipalFromRequest(r)
		run := r.PathValue("run")
		if !ok || p.Issuer != WorkflowParentPrincipalIssuer || p.WorkflowParent == nil || !apiv1.ValidRunID(run) || p.Subject != "run:"+run || !blobstore.ValidDigest(p.WorkflowParent.ContractDigest) {
			writeError(w, http.StatusForbidden, "parent_pod_required", "an exact contained parent attempt is required")
			return
		}
		if service == nil {
			writeError(w, http.StatusForbidden, "parent_access_unavailable", "parent access is unavailable")
			return
		}
		if status, code, message := validateMutationTransport(r); status != 0 {
			writeError(w, status, code, message)
			return
		}
		digest, err := childStringBody(r, "contractDigest", 71)
		if err != nil || digest != p.WorkflowParent.ContractDigest {
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

func parentAccessPath(path string) bool {
	run, ok := strings.CutPrefix(path, "/api/v1/runs/")
	if !ok {
		return false
	}
	run, ok = strings.CutSuffix(run, "/child-workflow-access")
	return ok && apiv1.ValidRunID(run)
}
