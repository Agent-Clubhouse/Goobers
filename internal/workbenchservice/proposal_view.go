package workbenchservice

import (
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

func proposalView(record triggerqueue.WorkbenchProposal, duplicate bool) workbench.MetadataProposalCommand {
	input := record.Input
	view := workbench.MetadataProposalCommand{ID: record.ID, Gaggle: input.Scope.Gaggle, SourceBindingID: input.Scope.SourceBindingID, Actor: workbench.CommandActor{Issuer: input.Scope.Actor.Issuer, Subject: input.Scope.Actor.Subject}, Path: input.Request.Path, State: record.State, Duplicate: duplicate, RequestDigest: record.RequestDigest, OperationDigest: input.OperationDigest, Expected: input.Request.Expected, AcceptedAt: record.AcceptedAt, CompletedAt: record.CompletedAt, Phases: []workbench.MetadataProposalPhase{}, Observations: []workbench.MetadataProposalObservation{}, OmittedObservations: record.OmittedObservations, NextAction: proposalNextAction(record.State)}
	if record.Plan != nil {
		view.ProposedContentDigest = record.Plan.Preview.ProposedContentDigest
		view.Branch = providers.RepositoryProposalBranch(record.Plan.Native.CommandID)
	}
	for _, phase := range record.Phases {
		value := workbench.MetadataProposalPhase{Name: phase.Name, Outcome: phase.Outcome, ClaimedAt: phase.ClaimedAt, FinishedAt: phase.FinishedAt}
		if value.Outcome == "" {
			value.Outcome = "pending"
		}
		if phase.Result != nil {
			value.TreeID = phase.Result.TreeID
			value.CommitID = phase.Result.CommitID
			value.PullRequest = proposalPRView(phase.Result.PullRequest)
		}
		view.Phases = append(view.Phases, value)
	}
	for _, observed := range record.Observations {
		view.Observations = append(view.Observations, workbench.MetadataProposalObservation{Phase: observed.Phase, At: observed.At, Found: observed.Result.Found, Matches: observed.Result.Matches, TreeID: observed.Result.TreeID, CommitID: observed.Result.CommitID, PullRequest: proposalPRView(observed.Result.PullRequest)})
	}
	return view
}
func proposalPRView(pr *providers.PullRequestResult) *workbench.MetadataProposalPR {
	if pr == nil {
		return nil
	}
	return &workbench.MetadataProposalPR{ID: pr.ID, Number: pr.Number, URL: pr.URL}
}
func proposalNextAction(state string) string {
	switch state {
	case "accepted", "prepared":
		return "The next phase has not begun. Explicitly submit the same command key to continue under current permissions."
	case "attempting", "unknown":
		return "Check the exact retained effect. Do not submit another command key to retry it. An observation never repeats or advances a write."
	case "confirmed":
		return "The provider acknowledged the draft pull request. Review its diff and use the repository's normal merge process."
	case "observed":
		return "The exact draft pull request was observed. Its original response remains uncertain; this receipt does not claim acknowledgement."
	case "not-applied":
		return "No visible branch or PR effect began. Refresh the source and check current permissions before making another proposal."
	case "blocked":
		return "A partial branch effect remains retained. Inspect the source; no further phase is automatically authorized."
	default:
		return "Inspect the retained proposal before another source change."
	}
}
func proposalError(err error) error {
	switch {
	case errors.Is(err, workbench.ErrMetadataPreview):
		return readError(http.StatusRequestEntityTooLarge, "workbench_preview_too_large", "The encoded source diff exceeds the bounded preview size.")
	case errors.Is(err, workbench.ErrMetadataRevision):
		return readError(http.StatusConflict, "workbench_source_changed", "The expected source commit or file changed. Refresh it before proposing another edit.")
	case errors.Is(err, workbench.ErrMetadataEdit):
		return readError(http.StatusForbidden, "workbench_proposal_denied", "The current source declaration does not permit this metadata edit.")
	case errors.Is(err, workbench.ErrMetadataEdge):
		return readError(http.StatusConflict, "workbench_relationship_conflict", "The relationship does not match its current declared owner and content.")
	case errors.Is(err, providers.ErrRepositoryProposal):
		return readError(http.StatusConflict, "workbench_proposal_unverifiable", "The exact proposal effect could not be verified. Its retained attempt remains unchanged.")
	default:
		return writeError(err)
	}
}
