package workbenchservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

// Repair accepts one bounded file intent under exact current authority. The
// bridge supplies a turn-scoped request key; no provider identity comes from it.
func (s *SessionPRRepair) Repair(ctx context.Context, key string, request sessioning.PRRepairRequest) (sessioning.PRRepairCommandView, error) {
	var result sessioning.PRRepairCommandView
	// Copy pointed contents before hashing, custody, or asynchronous policy cancel.
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > triggerqueue.MaxPRRepairIntentBytes || !s.service.exact(request) {
		return result, prRepairError(triggerqueue.ErrTransition)
	}
	if err = json.Unmarshal(raw, &request); err != nil {
		return result, prRepairError(err)
	}
	err = s.use(ctx, func(ctx context.Context, bound ReadBinding) error {
		input, err := s.input(key, request)
		if err != nil {
			return err
		}
		previous, done, err := s.replay(ctx, input)
		if err != nil {
			return err
		}
		if done {
			result = prRepairView(previous)
			return nil
		}
		client, err := s.adapter(ctx, bound)
		if err != nil {
			return err
		}
		target, err := s.inspect(ctx, client, request.ParentCommandID)
		if err != nil {
			return err
		}
		return s.service.WithCustody(ctx, target, func(ctx context.Context) error {
			record, err := s.repairLocked(ctx, client, input, target)
			result = prRepairView(record)
			return err
		})
	})
	return result, prRepairError(err)
}
func (s *SessionPRRepair) input(key string, request sessioning.PRRepairRequest) (triggerqueue.PRRepairCommandInput, error) {
	target, operation, err := triggerqueue.PRRepairDigests(s.scope(), s.selection, request)
	return triggerqueue.PRRepairCommandInput{Scope: s.scope(), RequestID: key, TargetDigest: target, OperationDigest: operation, Request: request, Origin: s.origin, Selection: s.selection}, err
}
func (s *SessionPRRepair) replay(ctx context.Context, input triggerqueue.PRRepairCommandInput) (triggerqueue.PRRepairCommand, bool, error) {
	previous, err := s.service.Queue.FindPRRepairCommand(ctx, input.Scope, input.RequestID)
	if errors.Is(err, sql.ErrNoRows) {
		return previous, false, nil
	}
	if err != nil && !errors.Is(err, triggerqueue.ErrWorkbenchCommandExpired) {
		return previous, false, err
	}
	if previous.Input.TargetDigest != input.TargetDigest {
		return previous, false, interactiveaccess.ErrDenied
	}
	if err != nil {
		return previous, false, err
	}
	previous, _, err = s.service.Queue.AcceptPRRepairCommand(ctx, input, s.service.now())
	return previous, previous.State != "accepted", err
}
func (s *SessionPRRepair) repairLocked(ctx context.Context, client PRRepairClient, input triggerqueue.PRRepairCommandInput, locked providers.RepairPullRequest) (triggerqueue.PRRepairCommand, error) {
	previous, done, err := s.replay(ctx, input)
	if err != nil || done {
		return previous, err
	}
	target, err := s.inspect(ctx, client, input.Request.ParentCommandID)
	if err != nil {
		return previous, err
	}
	if target.HeadSHA != input.Request.ExpectedHeadSHA || target.Head != locked.Head || target.Base != locked.Base {
		return previous, triggerqueue.ErrConflict
	}
	if _, err = s.authorize(ctx); err != nil {
		return previous, err
	}
	// Resolving a credential may register a secret that was not known at entry.
	if !s.service.exact(input.Request) {
		return previous, interactiveaccess.ErrDenied
	}
	input.Target = &target
	record, _, err := s.service.Queue.AcceptPRRepairCommand(ctx, input, s.service.now())
	if err != nil {
		return record, err
	}
	record, claimed, err := s.service.Queue.ClaimPRRepairCommand(ctx, input.Scope, record.ID, record.RequestDigest, s.service.now())
	if err != nil || !claimed {
		return record, err
	}
	return s.executeRepair(ctx, client, record)
}
func (s *SessionPRRepair) executeRepair(ctx context.Context, client PRRepairClient, record triggerqueue.PRRepairCommand) (triggerqueue.PRRepairCommand, error) {
	receipt := sessioning.PRRepairReceipt{OperationDigest: record.Input.OperationDigest, Outcome: "not-applied"}
	native, err := record.Native()
	if _, authErr := s.authorize(ctx); err == nil && authErr == nil {
		applied, _ := client.ApplyPullRequestRepair(ctx, native)
		receipt.MutationAttempted, receipt.ProviderAcknowledged = applied.MutationAttempted, applied.Acknowledged
		if applied.MutationAttempted {
			receipt.Outcome = "unknown"
			observed, _ := client.ObservePullRequestRepair(ctx, native)
			receipt.ObservedMatches, receipt.CommitID = observed.Matches, observed.CommitID
			if applied.Acknowledged {
				receipt.CommitID = applied.CommitID
				receipt.ObservedMatches = observed.Matches && applied.CommitID == observed.CommitID
			}
			if applied.Acknowledged && observed.Matches && applied.CommitID == observed.CommitID {
				receipt.Outcome = "confirmed"
			}
		}
	}
	// Policy changes and request cancellation join this bounded receipt commit
	// before either the live lease or exclusive host PR custody may be released.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return s.service.Queue.CompletePRRepairCommand(cleanup, record.Input.Scope, record.ID, record.RequestDigest, receipt, s.service.now())
}
