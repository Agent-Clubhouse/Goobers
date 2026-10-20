package interactive

import "time"

func (s *Service) commitCommand(p *preparedCommand, hash string) error {
	return s.mutate(func(state *diskState) error {
		current := state.Sessions[p.receipt.SessionID]
		firstEvent := len(current.Events)
		appendEvent(current, "command.accepted", p.receipt.TurnID, p.command.CommandID, map[string]any{"commandType": p.command.Type, "text": p.text})
		s.recordCommandEffect(current, p)
		for i := firstEvent; i < len(current.Events); i++ {
			current.Events[i].ActorSource = p.origin.source
		}
		p.receipt.Sequence = current.Session.Sequence
		current.Commands[p.command.CommandID] = storedCommand{hash, p.receipt}
		return nil
	})
}

func (s *Service) recordCommandEffect(current *sessionRecord, p *preparedCommand) {
	switch p.command.Type {
	case "message":
		s.recordMessageCommand(current, p)
	case "resume":
		s.recordResumeCommand(current, p)
	case "stop":
		s.recordStopCommand(current, p)
	case "child.request":
		s.recordChildRequestCommand(current, p)
	case "child.review":
		s.recordChildReviewCommand(current, p)
	}
}

func (s *Service) recordMessageCommand(current *sessionRecord, p *preparedCommand) {
	current.Session.Lease = s.newLease(*p.launch)
	current.Session.Status = "running"
	current.Session.ActiveTurnID = p.launch.TurnID
	current.Session.Turns = append(current.Session.Turns, Turn{ID: p.launch.TurnID, Text: p.text, Status: "running", Attempt: 1, Fence: p.launch.Fence, StartedAt: time.Now().UTC()})
	appendEvent(current, "turn.started", p.launch.TurnID, p.command.CommandID, map[string]any{"attempt": 1, "fence": p.launch.Fence})
}

func (s *Service) recordResumeCommand(current *sessionRecord, p *preparedCommand) {
	current.Session.CustodyPending = false
	for i := range current.Session.Children {
		if current.Session.Children[i].Lease != nil && current.Session.Children[i].Lease.Status == "custody_pending" {
			current.Session.Children[i].Lease.Status = "released"
		}
	}
	current.Session.Lease = s.newLease(*p.launch)
	current.Session.Status = "running"
	current.Session.Fence = p.launch.Fence
	current.Session.RecoveryReason = ""
	for i := range current.Session.Turns {
		if current.Session.Turns[i].ID == p.launch.TurnID {
			current.Session.Turns[i].Status = "running"
			current.Session.Turns[i].Attempt = p.launch.Attempt
			current.Session.Turns[i].Fence = p.launch.Fence
			current.Session.Turns[i].FinishedAt = nil
			current.Session.Turns[i].Error = ""
		}
	}
	appendEvent(current, "turn.started", p.launch.TurnID, p.command.CommandID, map[string]any{"attempt": p.launch.Attempt, "fence": p.launch.Fence, "resumed": true})
}

func (s *Service) recordStopCommand(current *sessionRecord, p *preparedCommand) {
	current.Session.StopRequested = true
	current.Session.Fence++
	current.Session.StopFencePending = s.opts.OnStop != nil
	current.Session.CustodyPending = current.Session.CustodyPending || len(p.custody) > 0
	current.Session.Status = "stopping"
	appendEvent(current, "session.stopping", current.Session.ActiveTurnID, p.command.CommandID, map[string]any{"fence": current.Session.Fence})
	for i := range current.Session.Children {
		if current.Session.Children[i].Status == "proposed" {
			current.Session.Children[i].Status = "cancelled"
		}
	}
	if len(s.active[p.receipt.SessionID]) == 0 && !current.Session.StopFencePending && !current.Session.CustodyPending {
		markStopped(current)
	}
}

func (s *Service) recordChildRequestCommand(current *sessionRecord, p *preparedCommand) {
	executionKind := "interactive-review"
	if p.command.WorkflowYAML != "" {
		executionKind = "interactive-workflow"
	}
	current.Session.Children = append(current.Session.Children, Child{WorkflowYAML: p.command.WorkflowYAML, WorkflowDigest: workflowDigest(p.command.WorkflowYAML), ActorSource: p.origin.source, SourceTurnID: p.origin.turnID, ExecutionKind: executionKind, ReadOnly: true, ID: p.receipt.ChildID, Prompt: p.text, Status: "proposed", RequestedAt: time.Now().UTC()})
	appendEvent(current, "child.proposed", p.receipt.TurnID, p.command.CommandID, map[string]any{"childId": p.receipt.ChildID, "prompt": p.text, "readOnly": true, "workflowYaml": p.command.WorkflowYAML, "workflowDigest": workflowDigest(p.command.WorkflowYAML), "sourceTurnId": p.origin.turnID})
}

func (s *Service) recordChildReviewCommand(current *sessionRecord, p *preparedCommand) {
	for i := range current.Session.Children {
		child := &current.Session.Children[i]
		if child.ID != p.command.ChildID {
			continue
		}
		now := time.Now().UTC()
		child.ReviewedAt = &now
		switch p.command.Decision {
		case "approve":
			child.Status = "running"
			child.Lease = s.newLease(*p.launch)
		case "deny":
			child.Status = "denied"
		case "accept":
			child.Status = "reviewed"
		}
	}
	kind := "child.reviewed"
	if p.command.Decision == "approve" {
		kind = "child.started"
	}
	appendEvent(current, kind, p.receipt.TurnID, p.command.CommandID, map[string]any{"childId": p.command.ChildID, "decision": p.command.Decision})
}
