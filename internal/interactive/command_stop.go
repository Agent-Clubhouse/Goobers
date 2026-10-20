package interactive

import (
	"context"
	"errors"
)

func (s *Service) recoverStoppedCommand(ctx context.Context, p *preparedCommand) error {
	s.mu.Unlock()
	var custodyErr error
	recoverer, ok := s.opts.Executor.(RecoveryExecutor)
	if !ok {
		custodyErr = errors.New("executor cannot verify prior writer custody")
	} else {
		for _, request := range p.custody {
			if err := recoverer.Recover(ctx, request); err != nil {
				custodyErr = err
				break
			}
		}
	}
	s.mu.Lock()
	return s.mutate(func(state *diskState) error {
		current := state.Sessions[p.receipt.SessionID]
		current.Session.CustodyPending = custodyErr != nil
		if custodyErr != nil {
			current.Session.RecoveryReason = s.clean(custodyErr.Error())
			appendEvent(current, "recovery.required", "", p.command.CommandID, map[string]any{"reason": current.Session.RecoveryReason})
		}
		if custodyErr == nil {
			for i := range current.Session.Children {
				if current.Session.Children[i].Status == "uncertain" {
					current.Session.Children[i].Status = "cancelled"
				}
			}
		}
		if !current.Session.CustodyPending && !current.Session.StopFencePending && len(s.active[p.receipt.SessionID]) == 0 && current.Session.Status == "stopping" {
			markStopped(current)
		}
		return nil
	})
}

func (s *Service) stopCommand(ctx context.Context, owner string, p *preparedCommand) error {
	// The session fence is already durable. Never hold the session lock
	// while acquiring the broker lock: broker admission checks us too.
	if s.opts.OnStop != nil {
		s.mu.Unlock()
		hookErr := s.opts.OnStop(ctx, owner, p.receipt.SessionID)
		s.mu.Lock()
		err := s.mutate(func(state *diskState) error {
			current := state.Sessions[p.receipt.SessionID]
			current.Session.StopFencePending = hookErr != nil
			if hookErr != nil {
				appendEvent(current, "auth.fence_pending", "", p.command.CommandID, map[string]any{"reason": s.clean(hookErr.Error())})
			} else if len(s.active[p.receipt.SessionID]) == 0 && !current.Session.CustodyPending && current.Session.Status == "stopping" {
				markStopped(current)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for _, run := range s.active[p.receipt.SessionID] {
		run.cancel()
	}
	return nil
}
