package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workflowcluster"
)

// LabsWorkflowFeaturesService builds projection-gated fleet snapshots.
type LabsWorkflowFeaturesService interface {
	WorkflowFeatures(context.Context, string) (workflowcluster.FleetSnapshot, error)
	WorkflowFeaturesAvailability(context.Context) error
}

// WithLabsWorkflowFeatures enables the fleet workflow-feature read route.
func WithLabsWorkflowFeatures(service LabsWorkflowFeaturesService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API Labs workflow feature service is required")
		}
		config.labsWorkflowFeatures = service
		return nil
	}
}

func registerLabsWorkflowFeaturesRoute(router *Router, service LabsWorkflowFeaturesService, auditLog *log.Logger) {
	router.Handle(apicontract.RouteLabsWorkflowFeatures, func(w http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "labs_unavailable", "Labs workflow features are unavailable")
			return
		}
		values := request.URL.Query()["features"]
		features := ""
		if len(values) == 1 {
			features = values[0]
		}
		if len(request.URL.Query()) != 1 || len(values) != 1 ||
			(features != workflowcluster.FleetFeatureStructural && features != workflowcluster.FleetFeatureBehavior) {
			writeError(w, http.StatusBadRequest, "invalid_features", "features must be exactly one of structural or behavior")
			return
		}
		snapshot, err := service.WorkflowFeatures(request.Context(), features)
		if err != nil {
			if errors.Is(err, workflowcluster.ErrFleetNotAssociated) ||
				errors.Is(err, workflowcluster.ErrFleetAssociationRevoked) ||
				errors.Is(err, workflowcluster.ErrFleetProjectionNotGranted) {
				writeError(w, http.StatusForbidden, "projection_not_granted", "the fleet association does not grant the labs.workflowFeatures projection")
				return
			}
			auditLog.Printf("labs workflow features failed: %v", err)
			writeError(w, http.StatusInternalServerError, "snapshot_error", "workflow feature snapshot could not be computed")
			return
		}
		principal := "local"
		if authenticated, ok := PrincipalFromRequest(request); ok && strings.TrimSpace(authenticated.Subject) != "" {
			principal = principalRef(authenticated)
		}
		auditLog.Printf("labs workflow features served principal=%q workflows=%d", principal, len(snapshot.Workflows))
		writeJSON(w, http.StatusOK, snapshot)
	})
}

func labsWorkflowFeaturesUnavailable(err error) (bool, string, string) {
	switch {
	case errors.Is(err, workflowcluster.ErrFleetNotAssociated):
		return false, "fleet_not_associated", "instance is not associated with a fleet"
	case errors.Is(err, workflowcluster.ErrFleetAssociationRevoked):
		return false, "fleet_association_revoked", "fleet association is revoked"
	case errors.Is(err, workflowcluster.ErrFleetProjectionNotGranted):
		return false, "projection_not_granted", "fleet association does not grant labs.workflowFeatures"
	default:
		return false, "association_unavailable", "fleet association state could not be read"
	}
}
