package workbenchservice

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/providers"
)

func commandView(record triggerqueue.WorkbenchCommand, duplicate bool) workbench.BacklogEditCommand {
	input := record.Input
	return workbench.BacklogEditCommand{ID: record.ID, Gaggle: input.Scope.Gaggle, SourceBindingID: input.Scope.SourceBindingID, Actor: workbench.CommandActor{Issuer: input.Scope.Actor.Issuer, Subject: input.Scope.Actor.Subject}, ItemID: input.Request.ID, SourceID: input.Request.SourceID, Field: string(input.Request.Field), State: record.State, Duplicate: duplicate, RequestDigest: record.RequestDigest, OperationDigest: input.OperationDigest, AcceptedAt: record.AcceptedAt, AttemptedAt: record.AttemptedAt, CompletedAt: record.CompletedAt, Receipt: record.Receipt, NextAction: writeNextAction(record.State)}
}
func writeNextAction(state string) string {
	switch state {
	case "accepted":
		return "The command is retained but has not claimed a provider attempt. Repeat the same command key to check it."
	case "attempting":
		return "An attempt may still be running or its response was lost. Check this receipt and inspect the source; do not submit a new command to retry it."
	case "unknown":
		return "The outcome is uncertain. Inspect the source and this evidence; a matching observation alone does not prove this command applied. Do not retry the write."
	case "confirmed":
		return "The provider acknowledged the change and the observed field matched. Refresh the item before making another edit."
	case "not-applied":
		return "No provider change was attempted or acknowledged. Refresh the item and check permissions before submitting a new command."
	default:
		return "This command requires inspection."
	}
}
func writeError(err error) error {
	if err == nil {
		return nil
	}
	var public *httpapi.InterventionError
	if errors.As(err, &public) {
		return public
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return readError(http.StatusGatewayTimeout, "workbench_write_interrupted", "The bounded command request did not complete. Keep its request key and inspect the receipt before another edit.")
	case errors.Is(err, triggerqueue.ErrWorkbenchCommandExpired):
		return readError(http.StatusGone, "workbench_command_expired", "The retained command receipt has expired. This request key remains reserved during its replay window.")
	case errors.Is(err, triggerqueue.ErrConflict):
		return readError(http.StatusConflict, "workbench_command_conflict", "This command key or retained receipt belongs to different input or source custody.")
	case errors.Is(err, triggerqueue.ErrFull):
		return readError(http.StatusTooManyRequests, "workbench_command_capacity", "Command custody is full. Existing uncertain edits must be inspected; they will not be discarded.")
	case errors.Is(err, sql.ErrNoRows):
		return readError(http.StatusNotFound, "workbench_command_not_found", "No command receipt is available for this actor and source.")
	case errors.Is(err, interactiveaccess.ErrDenied), errors.Is(err, workbenchprovider.ErrUnsupportedEdit):
		return readError(http.StatusForbidden, "workbench_write_denied", "Current permission or the source field allowlist does not permit this edit.")
	case errors.Is(err, triggerqueue.ErrTransition), errors.Is(err, providers.ErrNativeEdit):
		return readError(http.StatusBadRequest, "invalid_request", "The bounded native field edit is not valid.")
	default:
		return publicReadError(err)
	}
}
