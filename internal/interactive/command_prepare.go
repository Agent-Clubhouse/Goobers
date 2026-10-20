package interactive

import "context"

// preparedCommand keeps admission results separate from the durable mutation
// and dispatch. It is used only while the coordinator lock is held, except
// during the explicitly unlocked Stop callbacks.
type preparedCommand struct {
	command  Command
	origin   commandOrigin
	receipt  CommandReceipt
	text     string
	launch   *ExecutionRequest
	steerRun *activeRun
	custody  []ExecutionRequest
}

func (s *Service) prepareCommand(ctx context.Context, owner string, rec *sessionRecord, surface Surface, p *preparedCommand) error {
	switch p.command.Type {
	case "message":
		return s.prepareMessageCommand(owner, rec, surface, p)
	case "steer":
		return s.prepareSteerCommand(rec, p)
	case "stop":
		s.prepareStopCommand(owner, rec, p)
		return nil
	case "resume":
		return s.prepareResumeCommand(ctx, owner, rec, surface, p)
	case "child.request":
		return s.prepareChildRequestCommand(rec, surface, p)
	case "child.review":
		return s.prepareChildReviewCommand(owner, rec, surface, p)
	default:
		return problem(400, "invalid", "unknown interactive command type")
	}
}

func (s *Service) prepareMessageCommand(owner string, rec *sessionRecord, surface Surface, p *preparedCommand) error {
	if len(s.active[p.receipt.SessionID]) > 0 && rec.Session.Status == "idle" {
		return problem(409, "checkpoint_required", "wait for child snapshot work to finish before editing the parent")
	}
	if p.text == "" {
		return problem(400, "invalid", "message text is required")
	}
	if rec.Session.Status != "idle" && rec.Session.Status != "error" {
		return problem(409, "busy", "session must be idle before a new message")
	}
	if s.opts.Executor == nil {
		return problem(503, "executor_unavailable", "no interactive executor is configured")
	}
	if rec.Session.OutputBytes >= s.opts.MaxSessionOutputBytes {
		return problem(409, "capacity", "session output retention budget reached; create a new session")
	}
	if len(rec.Session.Turns) >= 1000 {
		return problem(409, "capacity", "prototype turn retention limit reached")
	}
	turnID := id("turn_")
	p.receipt.TurnID = turnID
	p.launch = &ExecutionRequest{SessionID: p.receipt.SessionID, TurnID: turnID, Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, ExecutionMode: rec.Session.ExecutionMode, Text: p.text, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence, Attempt: 1, History: clone(rec.Session.Turns), Instructions: surface.Instructions}
	return nil
}

func (s *Service) prepareSteerCommand(rec *sessionRecord, p *preparedCommand) error {
	if err := validateSteeringCommand(p.command, p.text, rec.Session); err != nil {
		return err
	}
	p.steerRun = s.active[p.receipt.SessionID][p.command.TurnID]
	if p.steerRun == nil {
		return problem(409, "stale_turn", "target attempt is unavailable")
	}
	if len(p.steerRun.steering) >= cap(p.steerRun.steering) {
		return problem(409, "capacity", "steering queue is full")
	}
	p.receipt.TurnID = p.command.TurnID
	return nil
}

func (s *Service) prepareStopCommand(owner string, rec *sessionRecord, p *preparedCommand) {
	if len(s.active[p.receipt.SessionID]) == 0 && (rec.Session.Status == "recovery_required" || rec.Session.CustodyPending) {
		base := ExecutionRequest{SessionID: p.receipt.SessionID, Owner: owner, SurfaceID: rec.Session.SurfaceID, Mode: rec.Session.Mode, ExecutionMode: rec.Session.ExecutionMode, TurnID: rec.Session.ActiveTurnID, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence}
		p.custody = append(p.custody, base)
		for _, child := range rec.Session.Children {
			if child.Status == "uncertain" {
				next := base
				next.ChildID = child.ID
				next.ReadOnly = true
				p.custody = append(p.custody, next)
			}
		}
	}
	p.receipt.TurnID = rec.Session.ActiveTurnID
}

func (s *Service) prepareResumeCommand(ctx context.Context, owner string, rec *sessionRecord, surface Surface, p *preparedCommand) error {
	if rec.Session.StopRequested {
		return problem(409, "invalid_state", "stop was requested; retry stop to verify remaining custody")
	}
	if rec.Session.Status != "recovery_required" && rec.Session.Status != "waiting_auth" {
		return problem(409, "invalid_state", "only waiting or interrupted sessions can resume")
	}
	if s.opts.Executor == nil {
		return problem(503, "executor_unavailable", "no interactive executor is configured")
	}
	if rec.Session.ActiveTurnID == "" {
		return problem(409, "recovery_required", "interrupted child requires operator reconciliation; no parent turn is resumable")
	}
	var turn *Turn
	for i := range rec.Session.Turns {
		if rec.Session.Turns[i].ID == rec.Session.ActiveTurnID {
			turn = &rec.Session.Turns[i]
		}
	}
	if turn == nil {
		return problem(409, "recovery_required", "interrupted turn is unavailable")
	}
	request := ExecutionRequest{SessionID: p.receipt.SessionID, TurnID: turn.ID, Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, ExecutionMode: rec.Session.ExecutionMode, Text: turn.Text, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence + 1, Attempt: turn.Attempt + 1, History: clone(rec.Session.Turns), Instructions: surface.Instructions}
	s.bindSurfaceRequest(&request, surface)
	if rec.Session.Status == "recovery_required" {
		recovery, ok := s.opts.Executor.(RecoveryExecutor)
		if !ok {
			return problem(409, "recovery_required", "executor cannot verify prior writer custody")
		}
		if err := recovery.Recover(ctx, request); err != nil {
			return problem(409, "recovery_required", s.clean(err.Error()))
		}
		for _, child := range rec.Session.Children {
			if child.Status == "uncertain" {
				childRequest := request
				childRequest.ChildID = child.ID
				childRequest.TurnID = child.ID
				childRequest.ReadOnly = true
				s.bindSurfaceRequest(&childRequest, surface)
				if err := recovery.Recover(ctx, childRequest); err != nil {
					return problem(409, "recovery_required", s.clean(err.Error()))
				}
			}
		}
	}
	p.launch = &request
	p.receipt.TurnID = turn.ID
	return nil
}

func (s *Service) prepareChildRequestCommand(rec *sessionRecord, surface Surface, p *preparedCommand) error {
	if p.text == "" {
		return problem(400, "invalid", "child prompt is required")
	}
	if rec.Session.Status != "idle" && rec.Session.Status != "running" {
		return problem(409, "invalid_state", "child cannot be requested in the current state")
	}
	if len(rec.Session.Children) >= surface.MaxChildren {
		return problem(409, "child_limit", "surface child budget is exhausted")
	}
	p.receipt.ChildID = id("child_")
	p.receipt.TurnID = p.origin.turnID
	return nil
}

func (s *Service) prepareChildReviewCommand(owner string, rec *sessionRecord, surface Surface, p *preparedCommand) error {
	var child *Child
	for i := range rec.Session.Children {
		if rec.Session.Children[i].ID == p.command.ChildID {
			child = &rec.Session.Children[i]
		}
	}
	if child == nil {
		return problem(404, "not_found", "child is unavailable")
	}
	p.receipt.ChildID = child.ID
	switch p.command.Decision {
	case "approve":
		if rec.Session.Status != "idle" {
			return problem(409, "checkpoint_required", "wait for the parent turn to finish before approving a child snapshot")
		}
		if child.Status != "proposed" {
			return problem(409, "invalid_state", "child is not eligible for approval")
		}
		if s.opts.Executor == nil {
			return problem(503, "executor_unavailable", "no interactive executor is configured")
		}
		p.receipt.TurnID = child.ID
		p.launch = &ExecutionRequest{SessionID: p.receipt.SessionID, TurnID: child.ID, ChildID: child.ID, Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, ExecutionMode: rec.Session.ExecutionMode, WorkflowYAML: child.WorkflowYAML, Text: child.Prompt, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence, Attempt: 1, History: clone(rec.Session.Turns), Instructions: surface.Instructions, ReadOnly: true}
	case "deny":
		if child.Status != "proposed" {
			return problem(409, "invalid_state", "only proposed children can be denied")
		}
	case "accept":
		if child.Status != "succeeded" && child.Status != "failed" {
			return problem(409, "invalid_state", "only completed child results can be reviewed")
		}
	default:
		return problem(400, "invalid", "child decision must be approve, deny or accept")
	}
	return nil
}
