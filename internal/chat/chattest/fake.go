// Package chattest provides credential-free driver and wire fixture tests.
package chattest

import (
	"context"
	"github.com/goobers/goobers/internal/chat"
	"sync"
)

type FakeDriver struct {
	Caps    chat.Capabilities
	Session *FakeSession
}

func (d *FakeDriver) ID() string { return "fake" }
func (d *FakeDriver) Probe(context.Context) (chat.Probe, error) {
	return chat.Probe{HandshakeOK: true, Capabilities: d.Caps}, nil
}
func (d *FakeDriver) Start(_ context.Context, o chat.StartOptions) (chat.Session, error) {
	s := &FakeSession{stream: chat.NewStream(o), caps: d.Caps, ceiling: o.Ceiling, level: o.Ceiling.Clamp(o.Level)}
	s.stream.Emit("session_started", map[string]any{"capabilities": d.Caps}, "")
	d.Session = s
	return s, nil
}

type FakeSession struct {
	mu      sync.Mutex
	stream  *chat.Stream
	caps    chat.Capabilities
	ceiling chat.Ceiling
	level   chat.Level
	closed  bool
}

func (s *FakeSession) NativeID() string                     { return "fake-session" }
func (s *FakeSession) Capabilities() chat.Capabilities      { return s.caps }
func (s *FakeSession) Events() <-chan chat.Event            { return s.stream.Events() }
func (s *FakeSession) Emit(t string, d map[string]any) bool { return s.stream.Emit(t, d, "") }
func (s *FakeSession) Send(_ context.Context, c []chat.ContentBlock, o chat.SendOptions) (chat.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", chat.ErrClosed
	}
	if _, err := chat.PlainText(c); err != nil {
		return "", err
	}
	delivery := o.Delivery
	if delivery == chat.Steer && !s.caps.Steer {
		delivery = chat.Queued
	}
	s.stream.Emit("command.delivered", map[string]any{"delivery": delivery}, o.CommandID)
	return delivery, nil
}
func (s *FakeSession) Interrupt(context.Context) error { return nil }
func (s *FakeSession) RespondPermission(context.Context, string, chat.PermissionResponse) error {
	return chat.ErrUnknownRequest
}
func (s *FakeSession) AnswerQuestion(context.Context, string, chat.QuestionAnswers) error {
	return chat.ErrUnknownRequest
}
func (s *FakeSession) SetLevel(_ context.Context, l chat.Level) (chat.Level, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.level = s.ceiling.Clamp(l)
	return s.level, nil
}
func (s *FakeSession) SetModel(context.Context, string) error { return chat.ErrUnsupported }
func (s *FakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.stream.Close()
	}
	return nil
}
