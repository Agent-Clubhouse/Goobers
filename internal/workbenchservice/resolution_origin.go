package workbenchservice

import (
	"context"
	"strings"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
)

func (s *SessionResolver) verifyOrigin(ctx context.Context) (workbench.NeedsHumanResolutionOrigin, error) {
	id := s.identity
	lineage := id.Session
	if lineage == nil || id.ValidateSessionLineage() != nil || id.Gaggle != s.retained.Name {
		return workbench.NeedsHumanResolutionOrigin{}, interactiveaccess.ErrDenied
	}
	turn, err := s.service.Queue.SessionTurn(ctx, lineage.AcceptanceID)
	if err != nil {
		return workbench.NeedsHumanResolutionOrigin{}, err
	}
	if turn.State != "running" || turn.Record.State != triggerqueue.Dispatched || turn.Record.RunID != id.RunID || turn.Session.State != sessioning.Running || turn.Session.ActiveTurnID != turn.ID || turn.ID != lineage.TurnID || turn.Session.ID != lineage.SessionID || turn.Session.Gaggle != id.Gaggle || turn.Message.ID != lineage.MessageID || turn.Message.Actor == nil || *turn.Message.Actor != s.actor || turn.Session.GooberDigest != id.GooberDigest || turn.Session.ConfigGeneration != id.ConfigGeneration || sessioning.Digest(turn.Record.Payload) != lineage.EnvelopeDigest {
		return workbench.NeedsHumanResolutionOrigin{}, interactiveaccess.ErrDenied
	}
	inputs, err := s.service.Queue.SessionInputs(ctx, lineage.AcceptanceID)
	if err != nil {
		return workbench.NeedsHumanResolutionOrigin{}, err
	}
	raw, err := inputs.Validate(id.RunID, id.Gaggle)
	if err != nil || sessioning.Digest(raw) != lineage.InputDigest {
		return workbench.NeedsHumanResolutionOrigin{}, interactiveaccess.ErrDenied
	}
	return workbench.NeedsHumanResolutionOrigin{RunID: id.RunID, SessionID: lineage.SessionID, TurnID: lineage.TurnID, MessageID: lineage.MessageID, MessageDigest: strings.TrimPrefix(sessioning.MessageDigest(turn.Message.Text, turn.Message.RepairTarget), "sha256:"), GooberDigest: id.GooberDigest}, nil
}
