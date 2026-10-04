package workbenchservice

import (
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

func prRepairView(record triggerqueue.PRRepairCommand) sessioning.PRRepairCommandView {
	i := record.Input
	return sessioning.PRRepairCommandView{ID: record.ID, SourceBindingID: i.Scope.SourceBindingID, State: record.State, RequestDigest: record.RequestDigest, OperationDigest: i.OperationDigest, SelectedHeadSHA: i.Selection.ExpectedHeadSHA, ExpectedHeadSHA: i.Request.ExpectedHeadSHA, ParentCommandID: i.Request.ParentCommandID, RunID: i.Origin.RunID, Actor: i.Scope.Actor, AcceptedAt: record.AcceptedAt, AttemptedAt: record.AttemptedAt, CompletedAt: record.CompletedAt, Receipt: record.Receipt}
}
func prRepairError(err error) error {
	if errors.Is(err, providers.ErrPRRepair) {
		return readError(http.StatusConflict, "pr_repair_source_changed", "The selected PR head, ownership, or bounded file evidence changed. Inspect the exact selected PR; do not retry an uncertain repair.")
	}
	return writeError(err)
}
