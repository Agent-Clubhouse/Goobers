package restartintent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type compactReplay struct {
	Version                        int
	Issuer, Subject, RequestDigest string
}

func requestDigest(request runner.StageRestartRequest) string {
	raw, _ := json.Marshal(request)
	return journal.Digest(raw)
}

// CompactRejected keeps command and principal identity after large context has
// passed its replay window. Only exact no-effect dispositions may be compacted.
func (s *Service) CompactRejected(ctx context.Context, r triggerqueue.Record) error {
	plan, err := s.Load(ctx, r)
	if err != nil {
		return err
	}
	request, err := runner.RetainedStageRestartRequest(plan)
	if err != nil {
		return err
	}
	authority, err := interactiveaccess.ParseRestartAuthority(plan.Continuation.Inputs[interactiveaccess.RestartAuthorityInputName])
	if err != nil {
		return err
	}
	replay, err := json.Marshal(compactReplay{Version: 1, Issuer: authority.Issuer, Subject: authority.Subject, RequestDigest: requestDigest(request)})
	if err != nil {
		return err
	}
	raw, err := s.Queue.HumanRestartPlan(ctx, r.ID)
	if err != nil {
		return err
	}
	return s.Queue.CompactHumanRestart(ctx, r.ID, raw, replay, s.Now())
}

// MatchRejectedReplay checks exact host-scoped identity and command, without
// requiring a deleted snapshot or authorizing a new execution.
func MatchRejectedReplay(r triggerqueue.HumanRestartReplay, issuer, subject, gaggle, source string, request runner.StageRestartRequest) error {
	var saved compactReplay
	if len(r.Replay) == 0 || len(r.Replay) > 4096 || json.Unmarshal(r.Replay, &saved) != nil || saved.Version != 1 {
		return errors.New("restartintent: invalid compact replay")
	}
	if r.Gaggle != gaggle || r.SourceRun != source || saved.Issuer != issuer || saved.Subject != subject {
		return interactiveaccess.ErrDenied
	}
	if r.Epoch != request.EpochID || saved.RequestDigest != requestDigest(request) {
		return triggerqueue.ErrConflict
	}
	return nil
}
