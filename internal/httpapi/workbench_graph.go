package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workbenchgraph"
)

// WorkbenchGraphService collects only server-selected, currently authorized
// source pages. There is deliberately no client graph or source-page input.
type WorkbenchGraphService interface {
	Graph(context.Context, Principal, string) (workbenchgraph.Graph, error)
}

// WithWorkbenchGraph installs the bounded human graph projection endpoint.
func WithWorkbenchGraph(service WorkbenchGraphService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("workbench graph service is required")
		}
		config.workbenchGraph = service
		return nil
	}
}
func registerWorkbenchGraphRoute(router *Router, config handlerConfig, errorLog *log.Logger) {
	router.Handle(apicontract.RouteWorkbenchGraph, workbenchGraphHandler(config.workbenchGraph, errorLog))
}
func workbenchGraphHandler(service WorkbenchGraphService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || len(r.PathValue("gaggle")) > 253 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Graph reads accept no query or body. Sources are selected by the server.")
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "workbench_unavailable", "Workbench graph browsing is unavailable on this server.")
			return
		}
		graph, err := service.Graph(r.Context(), p, r.PathValue("gaggle"))
		if err != nil {
			writePlaneError(w, errorLog, "workbench graph", err)
			return
		}
		writeJSON(w, http.StatusOK, graph)
	}
}
