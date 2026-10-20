package interactive

import (
	"encoding/json"
	"errors"
	"sync"
)

func isDriverControl(kind string) bool {
	switch kind {
	case "permission.respond", "question.answer", "session.set_model", "session.set_level", "session.set_effort":
		return true
	default:
		return false
	}
}

func validateDriverControl(cmd Command) error {
	if !validCommandID(cmd.CommandID) || cmd.TurnID == "" {
		return problem(400, "invalid", "commandId and exact turnId are required")
	}
	raw, err := json.Marshal(cmd)
	if err != nil || len(raw) > 32<<10 {
		return problem(400, "invalid", "driver control exceeds size limit")
	}
	switch cmd.Type {
	case "permission.respond":
		if cmd.Message != "" && cmd.OptionID != "reject_once" {
			return problem(400, "invalid", "message is supported only with denial")
		}
		if cmd.RequestID == "" || (cmd.OptionID != "allow_once" && cmd.OptionID != "reject_once") {
			return problem(400, "invalid", "permission response requires requestId and allow_once or reject_once")
		}
	case "question.answer":
		if cmd.RequestID == "" || len(cmd.Answers) == 0 {
			return problem(400, "invalid", "question response requires requestId and answers")
		}
	case "session.set_model":
		if cmd.Model == "" || len(cmd.Model) > 256 {
			return problem(400, "invalid", "model must be a bounded identifier")
		}
	case "session.set_level":
		switch cmd.Level {
		case "plan", "ask", "auto_edit", "auto", "full_access":
		default:
			return problem(400, "invalid", "unknown permission level")
		}
	case "session.set_effort":
		if len(cmd.Effort) > 64 {
			return problem(400, "invalid", "effort must be a bounded identifier")
		}
	default:
		return problem(400, "invalid", "unknown driver control")
	}
	return nil
}

// Only Command (the human admission path) routes here. Execution tool methods
// cannot manufacture a human permission response or session-setting command.
func (s *Service) driverControlLocked(owner, sessionID string, cmd Command) (CommandReceipt, error) {
	rec, err := s.record(owner, sessionID)
	if err != nil {
		return CommandReceipt{}, err
	}
	if err = validateDriverControl(cmd); err != nil {
		return CommandReceipt{}, err
	}
	hash := digest(cmd)
	if old, ok := rec.Commands[cmd.CommandID]; ok {
		if old.Digest != hash {
			return CommandReceipt{}, problem(409, "idempotency_conflict", "commandId was used with different content")
		}
		return old.Receipt, nil
	}
	if err = s.checkControlLocked(owner, sessionID); err != nil {
		return CommandReceipt{}, err
	}
	if rec.Session.Sequence != cmd.ExpectedSequence {
		return CommandReceipt{}, problem(409, "stale_sequence", "refresh session before submitting this command")
	}
	if rec.Session.Fence != cmd.ExpectedFence {
		return CommandReceipt{}, problem(409, "stale_fence", "execution fence changed")
	}
	if rec.Session.Status != "running" || rec.Session.ActiveTurnID != cmd.TurnID {
		return CommandReceipt{}, problem(409, "stale_turn", "target turn is no longer active")
	}
	if rec.Session.ExecutionMode == "local-tools" {
		return CommandReceipt{}, problem(409, "unsupported", "local tools have no native driver controls")
	}
	run := s.active[sessionID][cmd.TurnID]
	if run == nil || run.request.ChildID != "" {
		return CommandReceipt{}, problem(409, "stale_turn", "target attempt is unavailable")
	}
	if len(run.controls) >= cap(run.controls) || len(rec.Commands) >= 10000 {
		return CommandReceipt{}, problem(409, "capacity", "driver control queue is full")
	}
	encoded, _ := json.Marshal(cmd)
	if s.clean(string(encoded)) != string(encoded) {
		return CommandReceipt{}, problem(400, "secret_in_control", "driver control contains credential-shaped material")
	}
	receipt := CommandReceipt{CommandID: cmd.CommandID, SessionID: sessionID, TurnID: cmd.TurnID, Type: cmd.Type, Status: "accepted", RequestID: cmd.RequestID}
	err = s.mutate(func(state *diskState) error {
		current := state.Sessions[sessionID]
		appendEvent(current, "command.accepted", cmd.TurnID, cmd.CommandID, map[string]any{"commandType": cmd.Type, "by": "human", "requestId": cmd.RequestID})
		receipt.Sequence = current.Session.Sequence
		current.Commands[cmd.CommandID] = storedCommand{hash, receipt}
		return nil
	})
	if err != nil {
		return CommandReceipt{}, err
	}
	var once sync.Once
	run.controls <- DriverControl{Command: clone(cmd), Check: func() error {
		return s.CheckExecution(owner, sessionID, cmd.TurnID, run.request.LeaseID, cmd.ExpectedFence)
	}, Ack: func(actual string, err error) { once.Do(func() { s.ackDriverControl(sessionID, cmd, actual, err) }) }}
	return receipt, nil
}

func (s *Service) ackDriverControl(sessionID string, command Command, actual string, deliveryErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.mutate(func(state *diskState) error {
		rec := state.Sessions[sessionID]
		stored, ok := rec.Commands[command.CommandID]
		if !ok || stored.Receipt.Status != "accepted" {
			return nil
		}
		if rec.Session.Status != "running" || rec.Session.ActiveTurnID != command.TurnID || rec.Session.Fence != command.ExpectedFence {
			deliveryErr = errors.New("target attempt ended before control acknowledgement")
		}
		kind := "command.delivered"
		stored.Receipt.Status = "delivered"
		data := map[string]any{"commandType": command.Type, "by": "human", "requestId": command.RequestID}
		if deliveryErr != nil {
			kind = "command.rejected"
			stored.Receipt.Status = "rejected"
			data["reason"] = s.clean(deliveryErr.Error())
		} else if command.Type == "session.set_model" || command.Type == "session.set_level" || command.Type == "session.set_effort" {
			stored.Receipt.Actual = &actual
			data["actual"] = actual
		}
		appendEvent(rec, kind, command.TurnID, command.CommandID, data)
		stored.Receipt.Sequence = rec.Session.Sequence
		rec.Commands[command.CommandID] = stored
		return nil
	})
}

func rejectUnconfirmedCommands(rec *sessionRecord, req ExecutionRequest) {
	if req.ChildID != "" {
		return
	}
	for key, cmd := range rec.Commands {
		if cmd.Receipt.Status != "accepted" || cmd.Receipt.TurnID != req.TurnID {
			continue
		}
		kind := "command.rejected"
		if cmd.Receipt.Type == "steer" {
			kind = "steering.rejected"
		} else if !isDriverControl(cmd.Receipt.Type) {
			continue
		}
		cmd.Receipt.Status = "rejected"
		rec.Commands[key] = cmd
		appendEvent(rec, kind, req.TurnID, key, map[string]any{"reason": "attempt ended before delivery confirmation", "requestId": cmd.Receipt.RequestID, "commandType": cmd.Receipt.Type})
	}
}
