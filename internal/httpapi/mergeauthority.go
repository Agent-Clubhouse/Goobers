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
	authority, ok := service.(MergeAuthorityService)
	if !ok {
		return func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "merge_authority_unavailable", "current merge authority is unavailable")
		}
	}
	return journalJSONHandler(service, errorLog, journalRoute[journalclient.MergeAuthorityRequest, journalclient.MergeAuthorityResponse]{
		operation: "check current merge authority",
		action:    "check current merge authority",
		sharedFields: func(input *journalclient.MergeAuthorityRequest) (string, string) {
			return input.RunID, input.Gaggle
		},
		validate: func(w http.ResponseWriter, input *journalclient.MergeAuthorityRequest) bool {
			if strings.TrimSpace(input.Stage) == "" || (input.Capability != string(capability.GitHubPRMerge) && input.Capability != string(capability.ADOPRComplete)) {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "stage and a merge capability are required")
				return false
			}
			return true
		},
		call: authority.MergeAuthority,
	})
}
