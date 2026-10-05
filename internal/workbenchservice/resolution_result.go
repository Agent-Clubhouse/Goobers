package workbenchservice

import (
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
)

var errResolutionBlocked = errors.New("needs-human resolution has unresolved dependency coverage")
var errResolutionEvidence = errors.New("needs-human resolution evidence is not verified")

func resolutionError(err error) error {
	switch {
	case errors.Is(err, errResolutionBlocked):
		return readError(http.StatusConflict, "needs_human_still_blocked", "Known dependencies or incomplete evidence still block resolution. Inspect the item again.")
	case errors.Is(err, errResolutionEvidence):
		return readError(http.StatusConflict, "needs_human_evidence_changed", "Resolution evidence is missing or changed. Inspect again and cite the actual source or current human message.")
	default:
		return writeError(err)
	}
}
func resolutionView(record triggerqueue.NeedsHumanCommand, duplicate bool) workbench.NeedsHumanResolutionCommand {
	input := record.Input
	next := writeNextAction(record.State)
	if record.State == "confirmed" {
		next = "Only the needs-human marker was cleared. Other eligibility, claims, gates and PR state are unchanged; inspect them before starting work."
	}
	return workbench.NeedsHumanResolutionCommand{Assessment: input.Request, ID: record.ID, Gaggle: input.Scope.Gaggle, SourceBindingID: input.Scope.SourceBindingID, Actor: workbench.CommandActor{Issuer: input.Scope.Actor.Issuer, Subject: input.Scope.Actor.Subject}, ItemID: input.Request.ID, SourceID: input.Request.SourceID, State: record.State, Duplicate: duplicate, RequestDigest: record.RequestDigest, OperationDigest: input.OperationDigest, Origin: input.Origin, AcceptedAt: record.AcceptedAt, AttemptedAt: record.AttemptedAt, CompletedAt: record.CompletedAt, Receipt: record.Receipt, NextAction: next}
}
