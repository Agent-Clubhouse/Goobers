package main

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/runner"
)

// MergeAuthority derives workflow and generation from the daemon's journal;
// neither an old worker tree nor caller-supplied workflow can authorize a merge.
func (s *daemonRunJournalService) MergeAuthority(ctx context.Context, request journalclient.MergeAuthorityRequest) (journalclient.MergeAuthorityResponse, error) {
	denied := journalclient.MergeAuthorityResponse{}
	if !s.runJournalGaggleOK(request.Gaggle, request.RunID) {
		return denied, gaggleMismatch("merge authority")
	}
	reader, err := journal.OpenRead(filepath.Join(s.layout.ForGaggle(request.Gaggle).RunsDir(), request.RunID))
	if err != nil {
		return denied, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return denied, err
	}
	machine, err := runner.PinnedWorkflowMachine(reader, identity)
	if err != nil {
		return denied, err
	}
	var defs credentialPlaneDefinitions
	if identity.ConfigGeneration != "" {
		pinned, release, pinErr := pinnedCredentialDefinitions(ctx, s.layout, identity.ConfigGeneration)
		if pinErr != nil {
			return denied, pinErr
		}
		defer release()
		defs = pinned
	} else {
		set, _, loadErr := loadConfigDirectory(s.layout.ConfigDir())
		if loadErr != nil {
			return denied, loadErr
		}
		defs = credentialPlaneDefinitionsFromSet(set)
	}
	profile, err := stageCredentialProfile(machine, defs, request.Stage, func() (map[string][]string, bool, error) {
		return runner.PinnedGateGooberCapabilities(reader, identity)
	})
	if err != nil {
		return denied, err
	}
	if !slices.Contains(profile.capabilities, request.Capability) {
		return denied, httpapi.NewInterventionError(http.StatusForbidden, "capability_undeclared", "merge capability is not declared by the admitted stage", nil)
	}
	// Remote runs, including legacy runs, always consult current authority.
	env := apiv1.InvocationEnvelope{RunID: request.RunID, TaskID: request.Stage, Gaggle: identity.Gaggle, WorkflowID: identity.Workflow, Capabilities: []string{request.Capability}}
	if err := checkCurrentMergeAuthority(s.layout, env); err != nil {
		return denied, httpapi.NewInterventionError(http.StatusForbidden, "merge_authority_revoked", err.Error(), nil)
	}
	return journalclient.MergeAuthorityResponse{Allowed: true}, nil
}
