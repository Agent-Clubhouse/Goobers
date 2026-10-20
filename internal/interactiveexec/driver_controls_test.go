package interactiveexec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/interactive"
)

type controlSession struct {
	chat.Session
	permission chat.PermissionResponse
	answers    chat.QuestionAnswers
	calls      int
}

func (s *controlSession) Capabilities() chat.Capabilities { return chat.MinimalCapabilities() }
func (s *controlSession) RespondPermission(_ context.Context, _ string, response chat.PermissionResponse) error {
	s.permission = response
	s.calls++
	return nil
}
func (s *controlSession) AnswerQuestion(_ context.Context, _ string, answers chat.QuestionAnswers) error {
	s.answers = answers
	s.calls++
	return nil
}
func (s *controlSession) SetLevel(_ context.Context, level chat.Level) (chat.Level, error) {
	return chat.NewCeiling(chat.Ask, chat.Ask, chat.Ask).Clamp(level), nil
}

func TestDriverControlsDeadlineAndAttribution(t *testing.T) {
	s := &controlSession{}
	c := newDriverControls(s, time.Millisecond)
	for _, kind := range []string{"permission_request", "question"} {
		event := chat.Event{Type: kind, Data: map[string]any{"requestId": kind}}
		if err := c.observe(&event); err != nil || event.Data["deadline"] == nil {
			t.Fatalf("request deadline: %+v, %v", event, err)
		}
	}
	if err := c.expire(context.Background(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if s.calls != 2 || s.permission.OptionID != "reject_once" || s.answers != nil {
		t.Fatalf("timeout did not deny/cancel both requests: %+v", s)
	}
	for _, kind := range []string{"permission", "question"} {
		id := kind
		if kind == "permission" {
			id = "permission_request"
		}
		event := chat.Event{Type: kind + "_resolved", Data: map[string]any{"requestId": id}}
		if err := c.observe(&event); err != nil || event.Data["by"] != "timeout" {
			t.Fatalf("timeout attribution: %+v, %v", event, err)
		}
	}
	if err := c.expire(context.Background(), time.Now().Add(time.Hour)); err != nil || s.calls != 2 {
		t.Fatal("expired request was sent twice")
	}
}

func TestDriverControlsFenceCeilingAndResponse(t *testing.T) {
	s := &controlSession{}
	c := newDriverControls(s, time.Minute)
	event := chat.Event{Type: "permission_request", Data: map[string]any{"requestId": "p"}}
	if err := c.observe(&event); err != nil {
		t.Fatal(err)
	}
	command := interactive.Command{Type: "permission.respond", RequestID: "p", OptionID: "allow_once", CommandID: "human-1"}
	fenced := errors.New("stale lease")
	c.dispatch(context.Background(), interactive.DriverControl{Command: command, Check: func() error { return fenced }, Ack: func(_ string, err error) {
		if !errors.Is(err, fenced) {
			t.Fatalf("fence error = %v", err)
		}
	}})
	if s.calls != 0 {
		t.Fatal("stale human command reached native session")
	}
	command.Message = "denial explanation"
	if _, err := c.apply(context.Background(), command); !errors.Is(err, chat.ErrUnsupported) {
		t.Fatalf("unadvertised denial extras: %v", err)
	}
	command.Message = ""
	if _, err := c.apply(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	resolved := chat.Event{Type: "permission_resolved", Data: map[string]any{"requestId": "p"}}
	if err := c.observe(&resolved); err != nil || resolved.Data["by"] != "human" || resolved.CommandID != command.CommandID {
		t.Fatalf("human attribution: %+v, %v", resolved, err)
	}
	if _, err := c.apply(context.Background(), command); !errors.Is(err, chat.ErrUnknownRequest) {
		t.Fatalf("duplicate response = %v", err)
	}
	actual, err := c.apply(context.Background(), interactive.Command{Type: "session.set_level", Level: string(chat.FullAccess)})
	if err != nil || actual != string(chat.Ask) {
		t.Fatalf("host must report driver clamped level: %q, %v", actual, err)
	}
}
