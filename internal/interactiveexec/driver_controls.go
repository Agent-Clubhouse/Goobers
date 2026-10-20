package interactiveexec

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/interactive"
)

type pendingRequest struct {
	kind     string
	deadline time.Time
}
type controlOrigin struct{ by, commandID string }
type driverControls struct {
	session chat.Session
	timeout time.Duration
	pending map[string]pendingRequest
	origins map[string]controlOrigin
}

func newDriverControls(session chat.Session, timeout time.Duration) *driverControls {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &driverControls{session: session, timeout: timeout, pending: map[string]pendingRequest{}, origins: map[string]controlOrigin{}}
}
func (c *driverControls) observe(event *chat.Event) error {
	id, _ := event.Data["requestId"].(string)
	switch event.Type {
	case "permission_request", "question":
		if id == "" {
			return fmt.Errorf("driver emitted a request without identity")
		}
		if len(c.pending) >= 64 {
			return fmt.Errorf("driver pending request capacity exhausted")
		}
		deadline := time.Now().Add(c.timeout)
		c.pending[id] = pendingRequest{event.Type, deadline}
		event.Data["deadline"] = deadline.UTC().Format(time.RFC3339Nano)
	case "permission_resolved", "question_resolved":
		origin, ok := c.origins[id]
		if !ok {
			origin.by = "policy"
		}
		event.Data["by"] = origin.by
		if origin.commandID != "" {
			event.CommandID = origin.commandID
		}
		delete(c.pending, id)
		delete(c.origins, id)
	}
	return nil
}
func (c *driverControls) dispatch(ctx context.Context, control interactive.DriverControl) {
	var actual string
	var err error
	if control.Check != nil {
		err = control.Check()
	}
	if err == nil {
		actual, err = c.apply(ctx, control.Command)
	}
	if control.Ack != nil {
		control.Ack(actual, err)
	}
}
func (c *driverControls) apply(ctx context.Context, command interactive.Command) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	switch command.Type {
	case "permission.respond", "question.answer":
		if command.Message != "" && !c.session.Capabilities().PermissionExtras.DenyMessage {
			return "", chat.ErrUnsupported
		}
		pending, ok := c.pending[command.RequestID]
		if !ok || (pending.kind == "question") != (command.Type == "question.answer") {
			return "", chat.ErrUnknownRequest
		}
		if !time.Now().Before(pending.deadline) {
			return "", fmt.Errorf("request deadline expired")
		}
		c.origins[command.RequestID] = controlOrigin{"human", command.CommandID}
		var err error
		if command.Type == "permission.respond" {
			err = c.session.RespondPermission(callCtx, command.RequestID, chat.PermissionResponse{OptionID: command.OptionID, Message: command.Message})
		} else {
			err = c.session.AnswerQuestion(callCtx, command.RequestID, chat.QuestionAnswers(command.Answers))
		}
		if err != nil {
			delete(c.origins, command.RequestID)
		} else {
			delete(c.pending, command.RequestID)
		}
		return "", err
	case "session.set_model":
		return command.Model, c.session.SetModel(callCtx, command.Model)
	case "session.set_level":
		level, err := c.session.SetLevel(callCtx, chat.Level(command.Level))
		return string(level), err
	case "session.set_effort":
		setter, ok := c.session.(chat.EffortSetter)
		if !ok {
			return "", chat.ErrUnsupported
		}
		return command.Effort, setter.SetEffort(callCtx, command.Effort)
	default:
		return "", chat.ErrUnsupported
	}
}
func (c *driverControls) expire(ctx context.Context, now time.Time) error {
	for id, pending := range c.pending {
		if now.Before(pending.deadline) {
			continue
		}
		c.origins[id] = controlOrigin{by: "timeout"}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var err error
		if pending.kind == "permission_request" {
			err = c.session.RespondPermission(callCtx, id, chat.PermissionResponse{OptionID: "reject_once", Message: "Request expired by host policy"})
		} else {
			err = c.session.AnswerQuestion(callCtx, id, nil)
		}
		cancel()
		delete(c.pending, id)
		if err != nil && !errors.Is(err, chat.ErrUnknownRequest) {
			return fmt.Errorf("driver could not deny an expired request: %w", err)
		}
	}
	return nil
}
