package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
)

// ChildWorkflowMonitorService authorizes the retained run's gaggle before
// reading queue lineage. A run name or cursor carries no access authority.
type ChildWorkflowMonitorService interface {
	ListChildWorkflows(context.Context, Principal, string, string) (apicontract.ChildWorkflowPage, error)
}

// WithChildWorkflowMonitor installs the authorized read-only child projection.
func WithChildWorkflowMonitor(service ChildWorkflowMonitorService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("child workflow monitor is required")
		}
		config.childMonitor = service
		return nil
	}
}

func registerChildMonitorRoute(router *Router, config handlerConfig, errorLog *log.Logger) {
	router.Handle(apicontract.RouteChildWorkflowMonitor, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if config.childMonitor == nil {
			writeError(w, http.StatusServiceUnavailable, "child_monitor_unavailable", "Child workflow monitoring is unavailable on this server.")
			return
		}
		query := r.URL.Query()
		if len(query) > 1 || len(query["after"]) > 1 || (len(query) == 1 && len(query["after"]) == 0) || len(query.Get("after")) > 128 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Only one bounded after cursor is supported.")
			return
		}
		result, err := config.childMonitor.ListChildWorkflows(r.Context(), p, r.PathValue("run"), query.Get("after"))
		if err != nil {
			writePlaneError(w, errorLog, "list child workflows", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
