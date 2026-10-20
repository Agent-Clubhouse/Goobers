package interactive

func (s *Service) validateCommandPayload(cmd Command) error {
	if !validCommandID(cmd.CommandID) || len(cmd.Text) > 32768 {
		return problem(400, "invalid", "commandId and text size must be valid")
	}
	if len(cmd.WorkflowYAML) > 32<<10 {
		return problem(400, "invalid_workflow", "child workflow definition exceeds 32 KiB")
	}
	if cmd.WorkflowYAML != "" && cmd.Type != "child.request" {
		return problem(400, "invalid_workflow", "workflow definition is immutable and may only be supplied when proposing a child")
	}
	if cmd.WorkflowYAML != "" && s.clean(cmd.WorkflowYAML) != cmd.WorkflowYAML {
		return problem(400, "secret_in_workflow", "child workflow definition contains credential-shaped material")
	}
	return nil
}

func (s *Service) validateCommandPolicy(cmd Command, session Session, surface Surface, exists bool) error {
	reviewOnly := cmd.Type == "child.review" && (cmd.Decision == "deny" || cmd.Decision == "accept")
	if cmd.Type == "stop" || reviewOnly {
		return nil
	}
	if s.reloading {
		return problem(503, "policy_reloading", "surface policy reload is in progress")
	}
	if !s.executionModeAvailable(session.ExecutionMode) || !surfaceAllowsMode(surface, session.ExecutionMode) {
		return problem(409, "execution_mode_unavailable", "session execution mode is unpinned or unavailable; create a new session with an enabled mode")
	}

	if session.ConfigDigest != digest(surface) {
		return problem(409, "surface_changed", "surface policy changed; create a new session for the new configuration")
	}
	if !exists || !surface.Enabled {
		return problem(409, "surface_unavailable", "surface is disabled; stop remains available")
	}
	if session.StopFencePending {
		return problem(409, "invalid_state", "external authority fence is pending; retry stop before any new work")
	}
	return nil
}

func (s *Service) commandSurface(sessionID string, cmd Command, rec *sessionRecord) (Surface, error) {
	if rec.Session.Sequence != cmd.ExpectedSequence {
		return Surface{}, problem(409, "stale_sequence", "refresh session before submitting this command")
	}
	if rec.Session.Fence != cmd.ExpectedFence {
		return Surface{}, problem(409, "stale_fence", "execution fence changed")
	}
	surface, exists := s.surface(rec.Session.SurfaceID)
	if err := s.validateCommandPolicy(cmd, rec.Session, surface, exists); err != nil {
		return Surface{}, err
	}
	if cmd.Type == "resume" && len(s.active[sessionID]) > 0 {
		return Surface{}, problem(409, "recovery_required", "prior writer is still stopping")
	}
	if len(rec.Commands) >= 10000 {
		return Surface{}, problem(409, "capacity", "prototype command retention limit reached")
	}
	return surface, nil
}
