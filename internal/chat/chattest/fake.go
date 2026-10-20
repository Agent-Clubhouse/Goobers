// Package chattest provides credential-free driver and wire fixture tests.
package chattest

import (
	"context"
	"sync"

	"github.com/goobers/goobers/internal/chat"
)

// FakeDriver provides a credential-free controllable driver for host tests.
type FakeDriver struct {
	Caps    chat.Capabilities
	Session *FakeSession
}

// ID returns the transport identity.
func (d *FakeDriver) ID() string { return "fake" }

// Probe checks transport availability without sending a model prompt.
func (d *FakeDriver) Probe(context.Context) (chat.Probe, error) {
	return chat.Probe{HandshakeOK: true, Capabilities: d.Caps}, nil
}

// Start opens a session within the supplied host authority.
func (d *FakeDriver) Start(_ context.Context, o chat.StartOptions) (chat.Session, error) {
	s := &FakeSession{stream: chat.NewStream(o), caps: d.Caps, ceiling: o.Ceiling, level: o.Ceiling.Clamp(o.Level)}
	s.stream.Emit("session_started", map[string]any{"capabilities": d.Caps}, "")
	d.Session = s
	return s, nil
}

// FakeSession provides explicit event injection for host tests.
type FakeSession struct {
	mu      sync.Mutex
	stream  *chat.Stream
	caps    chat.Capabilities
	ceiling chat.Ceiling
	level   chat.Level
	closed  bool
}

// NativeID returns the native conversation identity.
func (s *FakeSession) NativeID() string { return "fake-session" }

// Capabilities advertises the operations implemented by a transport.
func (s *FakeSession) Capabilities() chat.Capabilities { return s.caps }

// Events returns the ordered event channel.
func (s *FakeSession) Events() <-chan chat.Event { return s.stream.Events() }

// Emit adds a normalized event, returning false if the stream cannot accept it.
func (s *FakeSession) Emit(t string, d map[string]any) bool { return s.stream.Emit(t, d, "") }

// Send accepts content and reports its actual delivery kind.
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

// Interrupt stops the current turn without granting new authority.
func (s *FakeSession) Interrupt(context.Context) error { return nil }

// RespondPermission answers a pending permission request by identity.
func (s *FakeSession) RespondPermission(context.Context, string, chat.PermissionResponse) error {
	return chat.ErrUnknownRequest
}

// AnswerQuestion answers a pending structured question by identity.
func (s *FakeSession) AnswerQuestion(context.Context, string, chat.QuestionAnswers) error {
	return chat.ErrUnknownRequest
}

// SetLevel requests a permission level bounded by host authority.
func (s *FakeSession) SetLevel(_ context.Context, l chat.Level) (chat.Level, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.level = s.ceiling.Clamp(l)
	return s.level, nil
}

// SetModel requests a model change when the transport supports it.
func (s *FakeSession) SetModel(context.Context, string) error { return chat.ErrUnsupported }

// Close closes the session or stream after releasing its owned resources.
func (s *FakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.stream.Close()
	}
	return nil
}
