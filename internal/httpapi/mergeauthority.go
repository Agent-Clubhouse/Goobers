package httpapi

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journalclient"
)

// MergeAuthorityService exposes only current permission, never grant material.
type MergeAuthorityService interface {
	MergeAuthority(context.Context, journalclient.MergeAuthorityRequest) (journalclient.MergeAuthorityResponse, error)
}

func journalMergeAuthorityHandler(service RunJournalService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		authority, ok := service.(MergeAuthorityService)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "merge_authority_unavailable", "current merge authority is unavailable")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input journalclient.MergeAuthorityRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if !validCrossRunRequest(w, input.RunID, input.Gaggle) {
			return
		}
		if strings.TrimSpace(input.Stage) == "" || (input.Capability != string(capability.GitHubPRMerge) && input.Capability != string(capability.ADOPRComplete)) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "stage and a merge capability are required")
			return
		}
		if !podBodyRunContained(w, request, input.RunID, "check current merge authority") {
			return
		}
		response, err := authority.MergeAuthority(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, "check current merge authority", err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}
