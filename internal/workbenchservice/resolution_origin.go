package workbenchservice

import (
	"context"
	"strings"

	"github.com/goobers/goobers/internal/workbench"
)

func (s *SessionResolver) verifyOrigin(ctx context.Context) (workbench.NeedsHumanResolutionOrigin, error) {
	origin, _, err := verifySessionOperationOrigin(ctx, s.service.Queue, s.identity, s.retained.Name, s.actor)
	if err != nil {
		return workbench.NeedsHumanResolutionOrigin{}, err
	}
	return workbench.NeedsHumanResolutionOrigin{RunID: origin.RunID, SessionID: origin.SessionID, TurnID: origin.TurnID, MessageID: origin.MessageID, MessageDigest: strings.TrimPrefix(origin.MessageDigest, "sha256:"), GooberDigest: origin.GooberDigest}, nil
}
