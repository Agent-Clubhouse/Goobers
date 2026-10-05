package httpapi

import (
	"context"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

// SessionResolutionOperations is the dedicated marker plane. A generic native
// field writer does not satisfy or inherit these inspected control operations.
type SessionResolutionOperations interface {
	InspectNeedsHuman(context.Context, string, string, sessioning.BacklogReadRequest) (workbench.NeedsHumanObservation, error)
	ResolveNeedsHuman(context.Context, string, string, sessioning.NeedsHumanResolutionRequest) (workbench.NeedsHumanResolutionCommand, error)
	NeedsHumanReceipt(context.Context, string, string, sessioning.BacklogReceiptRequest) (workbench.NeedsHumanResolutionCommand, error)
}

func callSessionResolution(ctx context.Context, service SessionOperationService, id apicontract.RouteID, token, run string, raw []byte) (any, error) {
	resolver, ok := service.(SessionResolutionOperations)
	if !ok {
		return nil, NewInterventionError(503, "session_resolution_unavailable", "Inspected blocker resolution is unavailable.", nil)
	}
	switch id {
	case apicontract.RouteSessionNeedsHumanInspect:
		request, err := sessioning.DecodeNeedsHumanInspect(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid stable blocker identity.")
		}
		return resolver.InspectNeedsHuman(ctx, token, run, request)
	case apicontract.RouteSessionNeedsHumanResolve:
		request, err := sessioning.DecodeNeedsHumanResolution(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid bounded resolution assessment.")
		}
		return resolver.ResolveNeedsHuman(ctx, token, run, request)
	case apicontract.RouteSessionNeedsHumanReceipt:
		request, err := sessioning.DecodeNeedsHumanReceipt(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid blocker receipt.")
		}
		return resolver.NeedsHumanReceipt(ctx, token, run, request)
	default:
		return nil, sessionBadRequest("Unknown session resolution operation.")
	}
}
func sessionResolutionRoute(id apicontract.RouteID) bool {
	switch id {
	case apicontract.RouteSessionNeedsHumanInspect, apicontract.RouteSessionNeedsHumanResolve, apicontract.RouteSessionNeedsHumanReceipt:
		return true
	}
	return false
}
