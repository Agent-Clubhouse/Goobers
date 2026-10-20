// Package execshim adapts existing per-turn harnesses to the chat contract.
// It cannot inject into a running turn. Hosts must Interrupt, await custody,
// then Send a new prompt and report interrupt-and-continue truthfully.
package execshim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
)

// Driver adapts a policy-bound per-turn harness to an interactive session.
type Driver struct {
	Adapter harness.Adapter
	Request harness.RunRequest
	Run     func(context.Context, harness.RunRequest) (harness.Outcome, error)
}

// ID identifies the per-turn compatibility transport.
func (d *Driver) ID() string { return "execshim" }
func capabilities() chat.Capabilities {
	c := chat.MinimalCapabilities()
	c.Interrupt = true
	c.ModelSwitch = "per_turn"
	return c
}

// Probe checks availability with the invocation's exact authentication config.
func (d *Driver) Probe(ctx context.Context) (chat.Probe, error) {
	p := chat.Probe{Capabilities: capabilities()}
	if d.Run != nil {
		p.HandshakeOK = true
		return p, nil
	}
	if d.Adapter == nil {
		return p, fmt.Errorf("execshim: adapter missing")
	}
	var info harness.PreflightInfo
	var err error
	if configured, ok := d.Adapter.(interface {
		PreflightConfig(context.Context, string, map[string]apiextensionsv1.JSON) (harness.PreflightInfo, error)
	}); ok {
		info, err = configured.PreflightConfig(ctx, d.Request.Model, d.Request.HarnessOptions)
	} else {
		info, err = d.Adapter.Preflight(ctx)
	}
	p.HandshakeOK = err == nil
	p.CLIVersion = info.Version
	return p, err
}

// Start refuses permission levels the underlying adapter cannot enforce.
func (d *Driver) Start(ctx context.Context, o chat.StartOptions) (chat.Session, error) {
	// Per-turn adapters have no interactive permission responder. Never claim
	// to enforce a planning or approval ceiling using prompt text alone.
	if o.Ceiling.Clamp(o.Level) != chat.Auto {
		return nil, fmt.Errorf("execshim requires auto level under the host policy: %w", chat.ErrUnsupported)
	}
	run := d.Run
	if run == nil {
		if d.Adapter == nil {
			return nil, fmt.Errorf("execshim: adapter missing")
		}
		run = d.Adapter.Run
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &session{ctx: ctx, cancel: cancel, stream: chat.NewStream(o), run: run, request: d.Request, level: o.Ceiling.Clamp(o.Level)}
	s.request.Model = o.Model
	s.emit("session_started", map[string]any{"driver": "execshim", "capabilities": capabilities()}, "")
	return s, nil
}

type session struct {
	nativeTurn uint64
	active     bool
	mu         sync.Mutex
	opMu       sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	stream     *chat.Stream
	run        func(context.Context, harness.RunRequest) (harness.Outcome, error)
	request    harness.RunRequest
	level      chat.Level
	closed     bool
	turnCancel context.CancelFunc
	turnDone   chan struct{}
	custodyErr error
	once       sync.Once
}

func (s *session) NativeID() string                { return "" }
func (s *session) Capabilities() chat.Capabilities { return capabilities() }
func (s *session) Events() <-chan chat.Event       { return s.stream.Events() }
func (s *session) Send(ctx context.Context, c []chat.ContentBlock, o chat.SendOptions) (chat.Delivery, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text, err := chat.PlainText(c)
	if err != nil {
		return "", err
	}
	if o.Delivery != "" && o.Delivery != chat.Prompt {
		return "", chat.ErrUnsupported
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", chat.ErrClosed
	}
	if s.turnDone != nil {
		select {
		case <-s.turnDone:
		default:
			return "", chat.ErrBusy
		}
	}
	if s.custodyErr != nil {
		return "", s.custodyErr
	}
	runCtx, cancel := context.WithCancel(s.ctx)
	runCtx, proof := invoke.WithWorkspaceQuiescence(runCtx)
	done := make(chan struct{})
	s.nativeTurn++
	s.active = true
	turnID := fmt.Sprint(s.nativeTurn)
	s.turnCancel = cancel
	s.turnDone = done
	request := s.request
	request.Envelope.Goal = text
	s.emit("command.accepted", nil, o.CommandID)
	s.emit("user_message", map[string]any{"content": c, "source": "user", "delivery": chat.Prompt}, o.CommandID)
	s.emit("command.delivered", map[string]any{"delivery": chat.Prompt}, o.CommandID)
	s.emit("turn_started", nil, "")
	go func() {
		defer close(done)
		defer cancel()
		out, runErr := s.run(runCtx, request)
		var custody error
		if proof.HasRegisteredWriters() || runErr == nil {
			custody = proof.Verify()
		}
		s.mu.Lock()
		s.custodyErr = errors.Join(s.custodyErr, custody)
		s.active = false
		s.mu.Unlock()
		if custody != nil {
			s.emit("custody.pending", nil, "")
		}
		if runErr != nil || custody != nil {
			reason := "error"
			if runCtx.Err() != nil {
				reason = "cancelled"
			}
			s.emit("error", map[string]any{"code": "adapter_failed", "message": "Harness invocation failed", "fatal": reason != "cancelled"}, "")
			s.emit("turn_ended", map[string]any{"reason": reason, "settled": custody == nil, "nativeTurnId": turnID}, "")
			return
		}
		s.emit("completion.accepted", map[string]any{"payload": json.RawMessage(out.Payload), "metrics": out.Metrics}, "")
		s.emit("turn_ended", map[string]any{"reason": "end_turn", "settled": true, "nativeTurnId": turnID}, "")
	}()
	return chat.Prompt, nil
}
func (s *session) ActiveTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return ""
	}
	return fmt.Sprint(s.nativeTurn)
}

func (s *session) Interrupt(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	cancel, done := s.turnCancel, s.turnDone
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.custodyErr
}
func (s *session) RespondPermission(context.Context, string, chat.PermissionResponse) error {
	return chat.ErrUnsupported
}
func (s *session) AnswerQuestion(context.Context, string, chat.QuestionAnswers) error {
	return chat.ErrUnsupported
}
func (s *session) SetLevel(context.Context, chat.Level) (chat.Level, error) {
	return s.level, chat.ErrUnsupported
}
func (s *session) SetModel(_ context.Context, m string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return chat.ErrClosed
	}
	s.request.Model = m
	return nil
}
func (s *session) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cancel()
		_ = s.Interrupt(context.Background())
		s.emit("session_ended", nil, "")
		s.stream.Close()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.custodyErr
}

func (s *session) emit(kind string, data map[string]any, id string) {
	if !s.stream.Emit(kind, data, id) {
		s.cancel()
	}
}
