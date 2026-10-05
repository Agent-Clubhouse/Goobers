package sessionops

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

// InspectNeedsHuman reports complete evidence, actual human instruction identity
// and wait reasons. Reading evidence never itself clears the marker.
func (b *Bridge) InspectNeedsHuman(ctx context.Context, token, run string, request sessioning.BacklogReadRequest) (result workbench.NeedsHumanObservation, err error) {
	if err = sessioning.ValidateNeedsHumanInspect(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "inspect_needs_human", request.SourceBindingID, request, &result, func(ctx context.Context, g *grant) (any, error) {
		return g.invocation.Resolver.Inspect(ctx, request.SourceBindingID, request.BacklogItemRequest)
	})
	return result, operationError(err)
}

// ResolveNeedsHuman retains one narrow marker receipt with the agent assessment.
func (b *Bridge) ResolveNeedsHuman(ctx context.Context, token, run string, request sessioning.NeedsHumanResolutionRequest) (result workbench.NeedsHumanResolutionCommand, err error) {
	if err = sessioning.ValidateNeedsHumanResolution(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "resolve_needs_human", request.SourceBindingID, request, &result, func(ctx context.Context, g *grant) (any, error) {
		return g.invocation.Resolver.Resolve(ctx, request.SourceBindingID, sessionCommandKey(g.invocation, request.RequestID), request.NeedsHumanResolutionRequest)
	})
	return result, operationError(err)
}

// NeedsHumanReceipt reads retained custody without another provider attempt.
func (b *Bridge) NeedsHumanReceipt(ctx context.Context, token, run string, request sessioning.BacklogReceiptRequest) (result workbench.NeedsHumanResolutionCommand, err error) {
	if err = sessioning.ValidateNeedsHumanReceipt(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "get_needs_human_receipt", request.SourceBindingID, request, &result, func(ctx context.Context, g *grant) (any, error) {
		return g.invocation.Resolver.Command(ctx, request.SourceBindingID, request.CommandID)
	})
	return result, operationError(err)
}
func resolutionOperation(operation string) bool {
	switch operation {
	case "inspect_needs_human", "resolve_needs_human", "get_needs_human_receipt":
		return true
	}
	return false
}
func invocationOperationOwner(inv Invocation, operation string) bool {
	if repairOperation(operation) {
		return inv.Repairer != nil
	}
	if resolutionOperation(operation) {
		return inv.Resolver != nil
	}
	return inv.Writer != nil
}
func validResolutionResult(value any, inv Invocation, binding string) error {
	switch result := value.(type) {
	case workbench.NeedsHumanObservation:
		if result.HumanInstruction == nil || result.HumanInstruction.Kind != "current-human-message" || result.HumanInstruction.ID != inv.Identity.Session.MessageID || len(result.Comments) > 100 || len(result.Dependencies) > 64 || len(result.LearnedDependencies) > 64 {
			return ErrDenied
		}
		digest, err := workbench.NeedsHumanObservationDigest(result)
		if err != nil || digest != result.Digest {
			return ErrDenied
		}
		return validateResult(result.Item, inv.Identity.Gaggle, binding)
	case workbench.NeedsHumanResolutionCommand:
		if sessioning.ValidateNeedsHumanReceipt(sessioning.BacklogReceiptRequest{SourceBindingID: binding, CommandID: result.ID}) != nil || result.Gaggle != inv.Identity.Gaggle || result.SourceBindingID != binding || result.Actor.Issuer != inv.Actor.Issuer || result.Actor.Subject != inv.Actor.Subject || !apiv1.ValidRunID(result.Origin.RunID) {
			return ErrDenied
		}
		if result.Receipt != nil && result.Receipt.Observed != nil {
			return validateResult(*result.Receipt.Observed, inv.Identity.Gaggle, binding)
		}
		return nil
	default:
		return validRepairResult(value, inv)
	}
}
