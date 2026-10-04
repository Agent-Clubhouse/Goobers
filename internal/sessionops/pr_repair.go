package sessionops

import (
	"context"

	"github.com/goobers/goobers/internal/sessioning"
)

// PRRepairer is host-bound to one actual human-selected PR, current permission,
// and immutable turn. It never accepts model-authored repository coordinates.
type PRRepairer interface {
	Target() sessioning.PRRepairTarget
	Inspect(context.Context, string) (sessioning.PRRepairInspection, error)
	ReadFile(context.Context, string, string) (sessioning.PRRepairFile, error)
	Repair(context.Context, string, sessioning.PRRepairRequest) (sessioning.PRRepairCommandView, error)
	Command(context.Context, string) (sessioning.PRRepairCommandView, error)
}

// RepairFactory returns nil when no explicit selected PR repair is permitted.
type RepairFactory func(context.Context, SourceContext) (PRRepairer, error)

// InspectSelectedPR reads the host-selected or confirmed descendant PR head.
func (b *Bridge) InspectSelectedPR(ctx context.Context, token, run string, request sessioning.PRRepairInspectRequest) (result sessioning.PRRepairInspection, err error) {
	if err = sessioning.ValidatePRRepairInspect(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "inspect_selected_pr", "", request, &result, func(ctx context.Context, g *grant) (any, error) {
		return g.invocation.Repairer.Inspect(ctx, request.ParentCommandID)
	})
	return result, operationError(err)
}

// ReadSelectedPRFile obtains bounded immutable file evidence for this selection.
func (b *Bridge) ReadSelectedPRFile(ctx context.Context, token, run string, request sessioning.PRRepairReadRequest) (result sessioning.PRRepairFile, err error) {
	if err = sessioning.ValidatePRRepairRead(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "read_selected_pr_file", "", request, &result, func(ctx context.Context, g *grant) (any, error) {
		return g.invocation.Repairer.ReadFile(ctx, request.Path, request.ParentCommandID)
	})
	return result, operationError(err)
}

// RepairSelectedPR retains actual command receipt evidence in this session journal.
func (b *Bridge) RepairSelectedPR(ctx context.Context, token, run string, request sessioning.PRRepairRequest) (result sessioning.PRRepairCommandView, err error) {
	if err = sessioning.ValidatePRRepairRequest(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "repair_selected_pr", "", request, &result, func(ctx context.Context, g *grant) (any, error) {
		return g.invocation.Repairer.Repair(ctx, sessionCommandKey(g.invocation, request.RequestID), request)
	})
	return result, operationError(err)
}

// PRRepairReceipt reads retained one-attempt custody without another provider write.
func (b *Bridge) PRRepairReceipt(ctx context.Context, token, run string, request sessioning.PRRepairReceiptRequest) (result sessioning.PRRepairCommandView, err error) {
	if err = sessioning.ValidatePRRepairReceipt(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "get_pr_repair_receipt", "", request, &result, func(ctx context.Context, g *grant) (any, error) {
		return g.invocation.Repairer.Command(ctx, request.CommandID)
	})
	return result, operationError(err)
}
func repairOperation(name string) bool {
	switch name {
	case "inspect_selected_pr", "read_selected_pr_file", "repair_selected_pr", "get_pr_repair_receipt":
		return true
	}
	return false
}
func validRepairResult(value any, inv Invocation) error {
	if inv.Repairer == nil {
		return ErrDenied
	}
	target := inv.Repairer.Target()
	switch result := value.(type) {
	case sessioning.PRRepairInspection:
		if result.Target != target {
			return ErrDenied
		}
		return nil
	case sessioning.PRRepairFile:
		if result.Target != target {
			return ErrDenied
		}
		return nil
	case sessioning.PRRepairCommandView:
		if sessioning.ValidatePRRepairReceipt(sessioning.PRRepairReceiptRequest{CommandID: result.ID}) != nil || result.SourceBindingID != target.SourceBindingID || result.Actor != inv.Actor || result.RunID != inv.Identity.RunID || result.SelectedHeadSHA != target.ExpectedHeadSHA {
			return ErrDenied
		}
		return nil
	}
	return ErrDenied
}
