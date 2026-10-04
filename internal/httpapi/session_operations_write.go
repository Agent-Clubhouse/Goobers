package httpapi

import (
	"context"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

// SessionWriteOperations is optional on a read-only installation. Every call
// repeats current actor, target and field checks in its existing execution lease.
type SessionWriteOperations interface {
	BacklogEditCapabilities(context.Context, string, string, sessioning.BacklogCapabilitiesRequest) (workbench.BacklogWriteCapabilities, error)
	EditBacklogItem(context.Context, string, string, sessioning.BacklogEditRequest) (workbench.BacklogEditCommand, error)
	BacklogEditReceipt(context.Context, string, string, sessioning.BacklogReceiptRequest) (workbench.BacklogEditCommand, error)
}

func sessionOperationName(name string) bool {
	switch name {
	case "inspect_needs_human", "resolve_needs_human", "get_needs_human_receipt", "get_backlog_item", "list_backlog_items", "get_backlog_edit_capabilities", "edit_backlog_item", "get_backlog_edit_receipt":
		return true
	}
	return false
}
func callSessionWrite(ctx context.Context, service SessionOperationService, id apicontract.RouteID, token, run string, raw []byte) (any, error) {
	writer, ok := service.(SessionWriteOperations)
	if !ok {
		return nil, NewInterventionError(503, "session_write_unavailable", "Session field edits are unavailable.", nil)
	}
	switch id {
	case apicontract.RouteSessionBacklogEditCapabilities:
		request, err := sessioning.DecodeBacklogCapabilities(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid capability request.")
		}
		return writer.BacklogEditCapabilities(ctx, token, run, request)
	case apicontract.RouteSessionBacklogEdit:
		request, err := sessioning.DecodeBacklogEdit(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid bounded field command.")
		}
		return writer.EditBacklogItem(ctx, token, run, request)
	case apicontract.RouteSessionBacklogReceipt:
		request, err := sessioning.DecodeBacklogReceipt(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid receipt request.")
		}
		return writer.BacklogEditReceipt(ctx, token, run, request)
	default:
		return nil, sessionBadRequest("Unknown session operation.")
	}
}
