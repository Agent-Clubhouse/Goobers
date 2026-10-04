package httpapi

import (
	"context"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/sessioning"
)

// SessionPRRepairOperations never accepts a model-authored repository or PR.
// The host service supplies the actual accepted human selection and live lease.
type SessionPRRepairOperations interface {
	InspectSelectedPR(context.Context, string, string, sessioning.PRRepairInspectRequest) (sessioning.PRRepairInspection, error)
	ReadSelectedPRFile(context.Context, string, string, sessioning.PRRepairReadRequest) (sessioning.PRRepairFile, error)
	RepairSelectedPR(context.Context, string, string, sessioning.PRRepairRequest) (sessioning.PRRepairCommandView, error)
	PRRepairReceipt(context.Context, string, string, sessioning.PRRepairReceiptRequest) (sessioning.PRRepairCommandView, error)
}

func callSessionPRRepair(ctx context.Context, service SessionOperationService, id apicontract.RouteID, token, run string, raw []byte) (any, error) {
	repair, ok := service.(SessionPRRepairOperations)
	if !ok {
		return nil, NewInterventionError(503, "session_pr_repair_unavailable", "Selected PR repair is unavailable.", nil)
	}
	switch id {
	case apicontract.RouteSessionPRRepairInspect:
		r, err := sessioning.DecodePRRepairInspect(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid selected PR inspection.")
		}
		return repair.InspectSelectedPR(ctx, token, run, r)
	case apicontract.RouteSessionPRRepairRead:
		r, err := sessioning.DecodePRRepairRead(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid selected PR file read.")
		}
		return repair.ReadSelectedPRFile(ctx, token, run, r)
	case apicontract.RouteSessionPRRepair:
		r, err := sessioning.DecodePRRepairRequest(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid bounded PR repair intent.")
		}
		return repair.RepairSelectedPR(ctx, token, run, r)
	case apicontract.RouteSessionPRRepairReceipt:
		r, err := sessioning.DecodePRRepairReceipt(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid PR repair receipt.")
		}
		return repair.PRRepairReceipt(ctx, token, run, r)
	}
	return nil, sessionBadRequest("Unknown selected PR repair operation.")
}
func sessionPRRepairRoute(id apicontract.RouteID) bool {
	switch id {
	case apicontract.RouteSessionPRRepairInspect, apicontract.RouteSessionPRRepairRead, apicontract.RouteSessionPRRepair, apicontract.RouteSessionPRRepairReceipt:
		return true
	}
	return false
}
