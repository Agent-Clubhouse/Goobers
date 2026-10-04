package sessionops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

// BacklogEditCapabilities authorizes current fields inside the live turn lease.
func (b *Bridge) BacklogEditCapabilities(ctx context.Context, token, run string, request sessioning.BacklogCapabilitiesRequest) (result workbench.BacklogWriteCapabilities, err error) {
	if err = sessioning.ValidateBacklogCapabilities(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "get_backlog_edit_capabilities", request.SourceBindingID, request, &result, func(ctx context.Context, g *grant) (any, error) {
		return g.invocation.Writer.Capabilities(ctx, request.SourceBindingID)
	})
	return result, operationError(err)
}

// EditBacklogItem executes at most the one attempt owned by its durable command.
func (b *Bridge) EditBacklogItem(ctx context.Context, token, run string, request sessioning.BacklogEditRequest) (result workbench.BacklogEditCommand, err error) {
	if err = sessioning.ValidateBacklogEdit(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "edit_backlog_item", request.SourceBindingID, request, &result, func(ctx context.Context, g *grant) (any, error) {
		key := sessionCommandKey(g.invocation, request.RequestID)
		return g.invocation.Writer.Patch(ctx, request.SourceBindingID, key, request.BacklogPatchRequest)
	})
	return result, operationError(err)
}

// BacklogEditReceipt returns retained custody without repeating a mutation.
func (b *Bridge) BacklogEditReceipt(ctx context.Context, token, run string, request sessioning.BacklogReceiptRequest) (result workbench.BacklogEditCommand, err error) {
	if err = sessioning.ValidateBacklogReceipt(request); err != nil {
		return result, operationError(err)
	}
	err = b.writeCall(ctx, token, run, "get_backlog_edit_receipt", request.SourceBindingID, request, &result, func(ctx context.Context, g *grant) (any, error) {
		return g.invocation.Writer.Command(ctx, request.SourceBindingID, request.CommandID)
	})
	return result, operationError(err)
}
func sessionCommandKey(inv Invocation, key string) string {
	encoded, _ := json.Marshal([]string{inv.Identity.Session.SessionID, inv.Identity.Session.TurnID, key})
	return "session-native:" + journal.Digest(encoded)
}

// writeCall retains returned command evidence even when policy cancellation
// raced its provider acknowledgement. Revocation still prevents returning data
// to the caller. The grant lock joins both the provider call and its host audit.
func (b *Bridge) writeCall(ctx context.Context, token, run, operation, binding string, request, result any, use func(context.Context, *grant) (any, error)) error {
	g, err := b.lookup(token)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err = b.active(g, run); err != nil {
		return err
	}
	if !invocationOperationOwner(g.invocation, operation) {
		return ErrDenied
	}
	if repairOperation(operation) {
		binding = g.invocation.Repairer.Target().SourceBindingID
	}
	// Reserve enough evidence budget before any possible external effect.
	if g.calls >= sessioning.MaxOperationsPerTurn || g.bytes+sessioning.MaxOperationResultBytes > sessioning.MaxOperationTurnBytes {
		return ErrLimit
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	stop := context.AfterFunc(g.invocation.Lease.Context(), cancel)
	defer stop()
	if err = ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	g.calls++
	note := operationNote(g, operation, binding, journal.Digest(raw))
	if err = g.invocation.Recorder.Append(note); err != nil {
		return err
	}
	value, callErr := use(ctx, g)
	output, evidenceErr := b.recordWriteEvidence(g, note, binding, value, callErr)
	if evidenceErr != nil {
		return evidenceErr
	}
	if callErr != nil {
		return errors.New("session command interrupted; preserve the request ID and inspect its receipt")
	}
	if err = b.active(g, run); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return json.Unmarshal(output, result)
}
func (b *Bridge) recordWriteEvidence(g *grant, note journal.Event, binding string, value any, callErr error) ([]byte, error) {
	note.Runner["kind"] = "session.operation.finished"
	note.Runner["outcome"] = "failed"
	var output []byte
	var err error
	if validWriteResult(value, g.invocation, binding) == nil {
		output, err = b.output(g, value)
		if err != nil {
			return nil, err
		}
		ref, recordErr := g.invocation.Recorder.RecordArtifactWithIntegrity(fmt.Sprintf("session-operations/%d/%d", g.invocation.StageSequence, g.calls), output, apiv1.IntegrityUnapproved)
		if recordErr != nil {
			return nil, recordErr
		}
		note.Artifacts = []journal.Ref{ref}
		note.Runner["resultDigest"] = journal.Digest(output)
		note.Runner["outcome"] = "read"
		if command, ok := value.(workbench.BacklogEditCommand); ok {
			note.Runner["commandId"] = command.ID
			note.Runner["outcome"] = command.State
			note.Runner["operationDigest"] = command.OperationDigest
			note.Runner["commandRequestDigest"] = command.RequestDigest
		}
		if command, ok := value.(workbench.NeedsHumanResolutionCommand); ok {
			note.Runner["commandId"] = command.ID
			note.Runner["outcome"] = command.State
			note.Runner["operationDigest"] = command.OperationDigest
			note.Runner["commandRequestDigest"] = command.RequestDigest
		}

		if command, ok := value.(sessioning.PRRepairCommandView); ok {
			note.Runner["commandId"] = command.ID
			note.Runner["outcome"] = command.State
			note.Runner["operationDigest"] = command.OperationDigest
			note.Runner["commandRequestDigest"] = command.RequestDigest
		}

	} else if callErr == nil {
		err = ErrDenied
	}
	if appendErr := g.invocation.Recorder.Append(note); appendErr != nil {
		return nil, appendErr
	}
	return output, err
}
func validWriteResult(value any, inv Invocation, binding string) error {
	switch result := value.(type) {
	case workbench.BacklogWriteCapabilities:
		if len(result.Fields) > 5 || len(result.Relationships) > 0 || result.ControlLabelChanges {
			return ErrDenied
		}
		return nil
	case workbench.BacklogEditCommand:
		if sessioning.ValidateBacklogReceipt(sessioning.BacklogReceiptRequest{SourceBindingID: binding, CommandID: result.ID}) != nil || result.Gaggle != inv.Identity.Gaggle || result.SourceBindingID != binding || result.Actor.Issuer != inv.Actor.Issuer || result.Actor.Subject != inv.Actor.Subject {
			return ErrDenied
		}
		if result.Receipt != nil && result.Receipt.Observed != nil {
			return validateResult(*result.Receipt.Observed, inv.Identity.Gaggle, binding)
		}
		return nil
	default:
		return validResolutionResult(value, inv, binding)
	}
}
