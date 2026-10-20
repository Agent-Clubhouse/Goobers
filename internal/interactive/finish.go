package interactive

import (
	"context"
	"errors"
	"time"
)

func (s *Service) finishCustody(rec *sessionRecord, req ExecutionRequest, result ExecutionResult) {
	lease := leaseFor(rec, req.ChildID)
	wasPolicyFenced := lease != nil && lease.LeaseID == req.LeaseID && lease.Status == "policy_fenced"
	if lease != nil && lease.LeaseID == req.LeaseID {
		if result.CustodyPending {
			lease.Status = "custody_pending"
		} else {
			lease.Status = "released"
		}
	}
	if wasPolicyFenced && !result.CustodyPending {
		pending := len(s.active[req.SessionID]) > 0
		if rec.Session.Lease != nil && rec.Session.Lease.Status == "custody_pending" {
			pending = true
		}
		for _, child := range rec.Session.Children {
			if child.Lease != nil && (child.Lease.Status == "custody_pending" || child.Lease.Status == "policy_fenced") {
				pending = true
			}
		}
		rec.Session.CustodyPending = pending
	}

	s.retainFinishedSnapshot(rec, req, result, lease)
}

func (s *Service) retainFinishedSnapshot(rec *sessionRecord, req ExecutionRequest, result ExecutionResult, lease *Lease) {
	// A fenced writer cannot publish more events or claim success. Its
	// final stopped-workspace snapshot is still custody evidence worth
	// retaining, including partial edits made before cancellation.
	if req.ChildID == "" && lease != nil && lease.LeaseID == req.LeaseID {
		if result.WorkspacePath != "" {
			rec.Session.WorkspacePath = result.WorkspacePath
		}
		if result.Changes != nil {
			rec.Session.Changes = clone(result.Changes)
			rec.Session.Diff = s.clean(result.Diff)
			appendEvent(rec, "changes.updated", req.TurnID, "", map[string]any{"changes": rec.Session.Changes, "diff": rec.Session.Diff, "writerStopped": !result.CustodyPending})
		}
	}
}

func (s *Service) finishPendingCustody(rec *sessionRecord, req ExecutionRequest, runErr error) {
	rec.Session.CustodyPending = true
	rec.Session.Status = "recovery_required"
	rec.Session.Fence++
	rec.Session.RecoveryReason = "Executor could not verify workspace quiescence; prior writer custody remains pending."
	for i := range rec.Session.Children {
		if rec.Session.Children[i].Status == "running" {
			rec.Session.Children[i].Status = "uncertain"
		}
	}

	if runErr != nil {
		rec.Session.RecoveryReason = s.clean(runErr.Error())
	}
	if req.ChildID == "" {
		rec.Session.ActiveTurnID = req.TurnID
		for i := range rec.Session.Turns {
			if rec.Session.Turns[i].ID == req.TurnID {
				rec.Session.Turns[i].Status = "interrupted"
				rec.Session.Turns[i].Error = rec.Session.RecoveryReason
				rec.Session.Turns[i].FinishedAt = nil
			}
		}
	} else {
		for i := range rec.Session.Children {
			if rec.Session.Children[i].ID == req.ChildID {
				rec.Session.Children[i].Status = "uncertain"
				rec.Session.Children[i].Result = rec.Session.RecoveryReason
			}
		}
	}
	appendEvent(rec, "recovery.required", req.TurnID, "", map[string]any{"reason": rec.Session.RecoveryReason, "custodyPending": true, "childId": req.ChildID})
}

func (s *Service) finishChild(rec *sessionRecord, req ExecutionRequest, result ExecutionResult, runErr error, current bool) {
	for i := range rec.Session.Children {
		child := &rec.Session.Children[i]
		if child.ID != req.ChildID {
			continue
		}
		if rec.Session.Status == "stopping" {
			child.Status = "cancelled"
		} else if current {
			child.Status = "succeeded"
			child.Result = s.clean(result.Message)
			if runErr != nil {
				child.Status = "failed"
				child.Result = s.clean(runErr.Error())
			}
			if result.WaitingAuth {
				child.Status = "failed"
				child.Result = "Authentication required; child did not complete."
			}
		}
		appendEvent(rec, "child.completed", req.TurnID, "", map[string]any{"childId": child.ID, "status": child.Status, "result": child.Result, "readOnly": true})
	}
}

func (s *Service) finishTurn(rec *sessionRecord, req ExecutionRequest, result ExecutionResult, runErr error, now time.Time) {
	status := "completed"
	rec.Session.Status = "idle"
	rec.Session.ActiveTurnID = ""
	if result.WaitingAuth {
		status = "waiting_auth"
		rec.Session.Status = "waiting_auth"
		rec.Session.ActiveTurnID = req.TurnID
		appendEvent(rec, "auth.required", req.TurnID, "", map[string]any{"reason": s.clean(result.AuthReason)})
	} else if runErr != nil {
		status = "failed"
		rec.Session.Status = "error"
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			status = "interrupted"
			rec.Session.Status = "recovery_required"
			rec.Session.ActiveTurnID = req.TurnID
			rec.Session.RecoveryReason = "Execution interrupted; writer stopped, explicit recovery required."
		}
		appendEvent(rec, "turn.failed", req.TurnID, "", map[string]any{"error": s.clean(runErr.Error()), "status": status})
	} else {
		if result.Message != "" {
			appendEvent(rec, "assistant.message", req.TurnID, "", map[string]any{"text": s.clean(result.Message)})
		}
		appendEvent(rec, "turn.completed", req.TurnID, "", map[string]any{"status": "completed"})
	}
	for i := range rec.Session.Turns {
		turn := &rec.Session.Turns[i]
		if turn.ID == req.TurnID {
			turn.Status = status
			turn.FinishedAt = &now
			if runErr != nil {
				turn.Error = s.clean(runErr.Error())
			}
		}
	}
}
