package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/sessioning"
)

// PRSelectionReader derives exact native identities from a human-chosen locator.
type PRSelectionReader interface {
	Inspect(context.Context, Principal, string, string, string) (sessioning.PRRepairInspection, error)
}

// WithPRSelection installs a read-only PR picker; it grants no repair permission.
func WithPRSelection(service PRSelectionReader) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("PR selection service is required")
		}
		config.prSelection = service
		return nil
	}
}

func prSelectionHandler(service PRSelectionReader, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
			writeError(w, 403, "interactive_access_denied", "PR selection requires a human identity.")
			return
		}
		if r.URL.RawQuery != "" || len(r.PathValue("source")) > 64 || !workbenchNativeID(r.PathValue("pullRequest")) {
			writeError(w, 400, CodeInvalidRequest, "Choose a configured source and PR number without extra query parameters.")
			return
		}
		if service == nil {
			writeError(w, 503, "pr_selection_unavailable", "Pull request inspection is unavailable on this server.")
			return
		}
		value, err := service.Inspect(r.Context(), p, r.PathValue("gaggle"), r.PathValue("source"), r.PathValue("pullRequest"))
		if err != nil {
			writePlaneError(w, errorLog, "PR selection", err)
			return
		}
		raw, err := json.Marshal(value)
		if err != nil || len(raw)+1 > 128<<10 {
			writeError(w, 502, "pr_selection_invalid", "The PR inspection exceeded its bounded response contract.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(raw, '\n'))
	}
}
