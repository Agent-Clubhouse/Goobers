package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workflowcluster"
)

// LabsWorkflowClusterService serves the Labs workflow-cluster read (#8309).
type LabsWorkflowClusterService interface {
	WorkflowClusters(context.Context) (workflowcluster.View, error)
}

// WithLabsWorkflowClusters enables the Labs workflow-cluster read route.
func WithLabsWorkflowClusters(service LabsWorkflowClusterService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API labs workflow cluster service is required")
		}
		config.labsWorkflowClusters = service
		return nil
	}
}

func registerLabsWorkflowClustersRoute(router *Router, service LabsWorkflowClusterService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteLabsWorkflowClusters, func(w http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "labs_unavailable", "workflow clusters are unavailable")
			return
		}
		view, err := service.WorkflowClusters(request.Context())
		if err != nil {
			errorLog.Printf("labs workflow clusters failed: %v", err)
			writeError(w, http.StatusInternalServerError, "read_error", "workflow clusters could not be computed")
			return
		}
		if view.TrendsErr != nil {
			errorLog.Printf("labs workflow cluster trends unavailable: %v", view.TrendsErr)
		}
		writeJSON(w, http.StatusOK, view)
	})
}
