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

// GetBacklogItem uses only the source adapter installed for this live turn.
func (b *Bridge) GetBacklogItem(ctx context.Context, token, run string, request sessioning.BacklogReadRequest) (result workbench.BacklogItem, err error) {
	if err = sessioning.ValidateBacklogRead(request); err != nil {
		return result, operationError(err)
	}
	err = b.call(ctx, token, run, "get_backlog_item", request.SourceBindingID, request, &result, func(ctx context.Context, reader BacklogReader) (any, error) {
		return reader.Get(ctx, request.SourceBindingID, request.BacklogItemRequest)
	})
	return result, operationError(err)
}

// ListBacklogItems fetches exactly one caller-requested source window.
func (b *Bridge) ListBacklogItems(ctx context.Context, token, run string, request sessioning.BacklogListRequest) (result workbench.BacklogPage, err error) {
	if err = sessioning.ValidateBacklogList(request); err != nil {
		return result, operationError(err)
	}
	err = b.call(ctx, token, run, "list_backlog_items", request.SourceBindingID, request, &result, func(ctx context.Context, reader BacklogReader) (any, error) {
		return reader.Page(ctx, request.SourceBindingID, request.BacklogPageRequest)
	})
	return result, operationError(err)
}
func (b *Bridge) call(ctx context.Context, token, run, operation, binding string, request, result any, use func(context.Context, BacklogReader) (any, error)) error {
	g, err := b.lookup(token)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err = b.active(g, run); err != nil {
		return err
	}
	if g.calls >= sessioning.MaxOperationsPerTurn || g.bytes >= sessioning.MaxOperationTurnBytes {
		return ErrLimit
	}
	ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
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
	value, operationErr := use(ctx, g.invocation.Reader)
	var refs []journal.Ref
	var output []byte
	if operationErr == nil {
		operationErr = ctx.Err()
	}
	if operationErr == nil {
		operationErr = validateResult(value, g.invocation.Identity.Gaggle, binding)
	}
	if operationErr == nil {
		output, operationErr = b.output(g, value)
		if operationErr == nil {
			ref, recordErr := g.invocation.Recorder.RecordArtifactWithIntegrity(fmt.Sprintf("session-operations/%d/%d", g.invocation.StageSequence, g.calls), output, apiv1.IntegrityUnapproved)
			operationErr = recordErr
			if recordErr == nil {
				refs = []journal.Ref{ref}
			}
		}
	}
	note = operationNote(g, operation, binding, journal.Digest(raw))
	note.Runner["kind"] = "session.operation.finished"
	note.Runner["outcome"] = "failed"
	if operationErr == nil {
		note.Runner["outcome"] = "read"
		note.Runner["resultDigest"] = journal.Digest(output)
	}
	note.Artifacts = refs
	if err = g.invocation.Recorder.Append(note); err != nil {
		return err
	}
	if operationErr != nil {
		if errors.Is(operationErr, ErrLimit) || errors.Is(operationErr, ErrDenied) {
			return operationErr
		}
		return errors.New("session source operation failed or was denied")
	}
	if err = b.active(g, run); err != nil {
		return err
	}
	return json.Unmarshal(output, result)
}
func (b *Bridge) output(g *grant, value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	limit := sessioning.MaxOperationResultBytes
	if _, item := value.(workbench.BacklogItem); item {
		limit = workbench.MaxBacklogItemBytes
	}
	if len(raw) > limit || g.bytes+len(raw) > sessioning.MaxOperationTurnBytes {
		return nil, ErrLimit
	}
	raw = b.Scrubber.Scrub(raw)
	if len(raw) > limit || g.bytes+len(raw) > sessioning.MaxOperationTurnBytes || !json.Valid(raw) {
		return nil, ErrLimit
	}
	g.bytes += len(raw)
	return raw, nil
}
func operationNote(g *grant, operation, binding, digest string) journal.Event {
	inv := g.invocation
	return journal.Event{Type: journal.EventRunnerAnnotation, Stage: "respond", Attempt: inv.Attempt, Runner: map[string]any{
		"kind": "session.operation.started", "operation": operation, "sourceBindingId": binding, "requestDigest": digest,
		"sessionId": inv.Identity.Session.SessionID, "turnId": inv.Identity.Session.TurnID, "humanIssuer": inv.Actor.Issuer, "humanSubject": inv.Actor.Subject,
		"stageSequence": inv.StageSequence, "commandSequence": g.calls,
	}}
}
