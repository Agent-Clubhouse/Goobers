package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workbench"
)

// WorkbenchDocumentService reads only server-selected repository source paths.
type WorkbenchDocumentService interface {
	Documents(context.Context, Principal, string, string, workbench.DocumentPageRequest) (workbench.DocumentPage, error)
}

// WithWorkbenchDocuments installs bounded source-owned objective/manifest reads.
func WithWorkbenchDocuments(service WorkbenchDocumentService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("workbench document service is required")
		}
		config.workbenchDocuments = service
		return nil
	}
}
func workbenchDocumentsHandler(service WorkbenchDocumentService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		principal, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "workbench_unavailable", "Repository source browsing is unavailable on this server.")
			return
		}
		page, _, err := workbenchReadRequest(r, apicontract.RouteWorkbenchDocuments)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Invalid repository source or pagination.")
			return
		}
		result, err := service.Documents(r.Context(), principal, r.PathValue("gaggle"), r.PathValue("source"), workbench.DocumentPageRequest(page))
		if err != nil {
			writePlaneError(w, errorLog, "workbench document read", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}
