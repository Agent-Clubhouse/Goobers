package workbenchservice

import (
	"context"
	"strings"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
)

func (s *SessionResolver) verifyEvidence(ctx context.Context, scope triggerqueue.WorkbenchCommandScope, request workbench.NeedsHumanResolutionRequest, observation workbench.NeedsHumanObservation) error {
	refs := append([]workbench.NeedsHumanEvidenceRef{request.Basis}, request.Evidence...)
	if len(refs) > 9 {
		return triggerqueue.ErrTransition
	}
	for _, ref := range refs {
		if !s.evidenceKnown(ctx, scope, request, observation, ref) {
			return errResolutionEvidence
		}
	}
	return nil
}
func (s *SessionResolver) evidenceKnown(ctx context.Context, scope triggerqueue.WorkbenchCommandScope, request workbench.NeedsHumanResolutionRequest, observation workbench.NeedsHumanObservation, ref workbench.NeedsHumanEvidenceRef) bool {
	switch ref.Kind {
	case "current-human-message":
		return ref.ID == s.origin.MessageID && ref.Digest == s.origin.MessageDigest
	case "learned-record":
		return ref.ID == request.ID && ref.Digest == observation.LearnedRecordDigest && strings.TrimSpace(observation.LearnedReason) != ""
	case "source-comment":
		for _, comment := range observation.Comments {
			if comment.ID == ref.ID && comment.Digest == ref.Digest {
				return true
			}
		}
	case "session-message":
		inputs, err := s.service.Queue.SessionInputs(ctx, s.identity.Session.AcceptanceID)
		if err != nil {
			return false
		}
		for _, message := range inputs.Messages {
			if message.ID == ref.ID && strings.TrimPrefix(sessioning.MessageDigest(message.Text, message.RepairTarget), "sha256:") == ref.Digest {
				return true
			}
		}
	case "native-command":
		command, err := s.service.Queue.WorkbenchCommand(ctx, scope, ref.ID)
		return err == nil && command.State == "confirmed" && command.Input.Request.ID == request.ID && command.Input.Request.SourceID == request.SourceID && command.Input.OperationDigest == ref.Digest
	}
	return false
}
