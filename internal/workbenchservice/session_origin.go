package workbenchservice

import (
	"context"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func verifySessionOperationOrigin(ctx context.Context, queue *triggerqueue.Store, id journal.RunIdentity, gaggle string, actor sessioning.Actor) (sessioning.PRRepairOrigin, *sessioning.PRRepairTarget, error) {
	lineage := id.Session
	if lineage == nil || id.ValidateSessionLineage() != nil || id.Gaggle != gaggle {
		return sessioning.PRRepairOrigin{}, nil, interactiveaccess.ErrDenied
	}
	turn, err := queue.SessionTurn(ctx, lineage.AcceptanceID)
	if err != nil {
		return sessioning.PRRepairOrigin{}, nil, err
	}
	if turn.State != "running" || turn.Record.State != triggerqueue.Dispatched || turn.Record.RunID != id.RunID || turn.Session.State != sessioning.Running || turn.Session.ActiveTurnID != turn.ID || turn.ID != lineage.TurnID || turn.Session.ID != lineage.SessionID || turn.Session.Gaggle != id.Gaggle || turn.Message.ID != lineage.MessageID || turn.Message.Actor == nil || *turn.Message.Actor != actor || turn.Session.GooberDigest != id.GooberDigest || turn.Session.ConfigGeneration != id.ConfigGeneration || sessioning.Digest(turn.Record.Payload) != lineage.EnvelopeDigest {
		return sessioning.PRRepairOrigin{}, nil, interactiveaccess.ErrDenied
	}
	inputs, err := queue.SessionInputs(ctx, lineage.AcceptanceID)
	if err != nil {
		return sessioning.PRRepairOrigin{}, nil, err
	}
	raw, err := inputs.Validate(id.RunID, id.Gaggle)
	if err != nil || sessioning.Digest(raw) != lineage.InputDigest {
		return sessioning.PRRepairOrigin{}, nil, interactiveaccess.ErrDenied
	}
	origin := sessioning.PRRepairOrigin{RunID: id.RunID, SessionID: lineage.SessionID, TurnID: lineage.TurnID, MessageID: lineage.MessageID, MessageDigest: sessioning.MessageDigest(turn.Message.Text, turn.Message.RepairTarget), ConfigGeneration: id.ConfigGeneration, GooberDigest: id.GooberDigest, EnvelopeDigest: lineage.EnvelopeDigest, InputDigest: lineage.InputDigest}
	return origin, sessioning.CopyPRRepairTarget(turn.Message.RepairTarget), nil
}
