package interactiveexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/chat/copilotsdk"
	"github.com/goobers/goobers/internal/chat/execshim"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
	"github.com/goobers/goobers/internal/interactiveagent"
)

func acknowledge(s interactive.Steering, delivery chat.Delivery, err error) {
	if s.AckDelivery != nil {
		s.AckDelivery(string(delivery), err)
	} else if s.Ack != nil {
		s.Ack(err)
	}
}

// driverRegistry is built from the pinned harness, never a cross-provider
// fallback. The legacy codex-local mode is the trusted local model host gate;
// the pinned harness now selects the transport independently of that name.
func (e *Executor) driverRegistry(provider string, request harness.RunRequest) *chat.Registry {
	if e.chatRegistry != nil {
		return e.chatRegistry(provider, request)
	}
	registry := chat.NewRegistry()
	var adapter harness.Adapter
	switch provider {
	case "copilot":
		c := &harness.CopilotAdapter{Command: []string{"copilot"}, EphemeralTmp: true, EphemeralTmpRoot: filepath.Join(request.Workspace, ".tmp"), DeferDiscovery: true}
		request.HarnessOptions = nil

		// Built-in shell expands to filesystem/test tools; host bridge tools remain
		// separately named and pass the same outer adapter permission policy.
		if e.runNative == nil {
			_ = registry.Register(provider, &copilotsdk.Driver{Adapter: c, Request: request})
		}
		adapter = c
	case "claude":
		request.HarnessOptions = nil
		c := &harness.ClaudeAdapter{Command: []string{"claude"}, EphemeralTmp: true, EphemeralTmpRoot: filepath.Join(request.Workspace, ".tmp")}
		if e.runNative == nil {
			_ = registry.Register(provider, c.ChatDriver(request))
		}
		adapter = c
	case "codex":
		c := &harness.CodexAdapter{Command: []string{"codex"}, EphemeralTmp: true, EphemeralTmpRoot: chatStatePath(e.config.StateDir, request.Workspace)}
		if e.runNative == nil {
			_ = registry.Register(provider, harness.NewCodexChatDriver(harness.CodexChatConfig{Adapter: c, Request: request, StateDir: filepath.Join(chatStatePath(e.config.StateDir, request.Workspace), "codex")}))
		}
		adapter = c
	}
	if adapter == nil {
		return registry
	}
	_ = registry.Register(provider, &execshim.Driver{Adapter: adapter, Request: request, Run: e.runNative})
	return registry
}

func (e *Executor) driveChat(ctx, runCtx context.Context, req interactive.ExecutionRequest, request harness.RunRequest, pending *interactive.Steering, emit func(string, map[string]any) error) (out harness.Outcome, restart *interactive.Steering, err error) {
	pendingAck := false
	defer func() {
		if pending != nil && !pendingAck {
			acknowledge(*pending, "", fmt.Errorf("replacement prompt was not delivered"))
		}
	}()
	provider := req.Harness
	if provider == "" {
		provider = "codex"
	}
	if provider == "copilot" {
		request.Tools = append([]string{"shell"}, req.Tools...)
		if req.ReadOnly {
			request.Tools = []string{"view", "grep", "glob"}
		}
	}
	registry := e.driverRegistry(provider, request)
	level := chat.Auto
	if req.ReadOnly {
		level = chat.Plan
	}
	resumePath := e.metadata(request.Workspace, "chat-"+provider+"-session")
	resumeID, _ := os.ReadFile(resumePath)
	s, attempts, err := registry.Start(runCtx, provider, chat.StartOptions{ResumeNativeID: string(resumeID), AgentID: req.Goober, SessionID: req.SessionID, TurnID: req.TurnID, Fence: req.Fence, Workspace: request.Workspace, Model: request.Model, SystemPrompt: request.Instructions, Level: level, Ceiling: chat.NewCeiling(level, level, level)})
	if err != nil {
		return out, nil, err
	}
	if id := s.NativeID(); id != "" {
		if saveErr := os.WriteFile(resumePath, []byte(id), 0600); saveErr != nil {
			return out, nil, errors.Join(saveErr, s.Close())
		}
	}
	controls := newDriverControls(s, e.config.DriverRequestTimeout)
	project := func(event chat.Event) error {
		if err := controls.observe(&event); err != nil {
			return err
		}
		if projected, ok := interactiveagent.ProjectChatEvent(event); ok {
			return emit(projected.Type, projected.Data)
		}
		return nil
	}
	defer func() {
		err = errors.Join(err, s.Close())
		for event := range s.Events() {
			if emitErr := project(event); emitErr != nil {
				err = errors.Join(err, emitErr)
				break
			}
		}
	}()
	if err := emit("execution.note", map[string]any{"driverAttempts": attempts}); err != nil {
		return out, nil, err
	}
	commandID := ""
	if pending != nil {
		commandID = pending.CommandID
	}
	delivered, err := s.Send(runCtx, chat.Text(request.Envelope.Goal), chat.SendOptions{Delivery: chat.Prompt, CommandID: commandID})
	if err != nil {
		return out, nil, err
	}
	if pending != nil {
		acknowledge(*pending, delivered, nil)
		pendingAck = true
	}
	return driveChatEvents(ctx, runCtx, req, request, s, controls, project)
}

func readChatCompletion(workspace, path string) ([]byte, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("chat completion is missing")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("chat completion exceeds limit")
	}
	return data, nil
}

func driveChatEvents(ctx, runCtx context.Context, req interactive.ExecutionRequest, request harness.RunRequest, s chat.Session, controls *driverControls, project func(chat.Event) error) (harness.Outcome, *interactive.Steering, error) {
	steering := req.Steering
	commands := req.Controls
	ticker := time.NewTicker(min(100*time.Millisecond, controls.timeout))
	defer ticker.Stop()
	completion := chatCompletion{}
	for {
		select {
		case <-ctx.Done():
			return completion.out, nil, ctx.Err()
		case now := <-ticker.C:
			if err := controls.expire(runCtx, now); err != nil {
				return completion.out, nil, err
			}
		case command, ok := <-commands:
			if !ok {
				commands = nil
				continue
			}
			controls.dispatch(runCtx, command)
		case steer, ok := <-steering:
			if !ok {
				steering = nil
				continue
			}
			restart, queued, err := deliverSteering(runCtx, s, steer)
			if queued {
				completion.queued++
			}
			if err != nil || restart != nil {
				return completion.out, restart, err
			}
		case event, ok := <-s.Events():
			if !ok {
				return completion.out, nil, fmt.Errorf("chat session ended before turn completion")
			}
			if err := project(event); err != nil {
				return completion.out, nil, err
			}
			settled, err := completion.observe(event, request, s)
			if err != nil || settled {
				return completion.out, nil, err
			}
		}
	}
}

func deliverSteering(ctx context.Context, s chat.Session, steer interactive.Steering) (*interactive.Steering, bool, error) {
	requested := chat.Delivery(steer.Delivery)
	if requested == "" {
		requested = chat.Steer
	}
	if requested == chat.Queued && s.Capabilities().Queue == "none" {
		acknowledge(steer, "", chat.ErrUnsupported)
		return nil, false, nil
	}
	if steer.SteeringMode == "interrupt-and-continue" || requested == chat.Steer && !s.Capabilities().Steer {
		if err := s.Close(); err != nil {
			acknowledge(steer, "", err)
			return nil, false, err
		}
		return &steer, false, nil
	}
	delivery, err := s.Send(ctx, chat.Text(steer.Text), chat.SendOptions{Delivery: requested, CommandID: steer.CommandID})
	acknowledge(steer, delivery, err)
	if errors.Is(err, chat.ErrClosed) {
		return nil, false, err
	}
	return nil, err == nil && delivery == chat.Queued, nil
}

type chatCompletion struct {
	out    harness.Outcome
	queued int
}

func (c *chatCompletion) observe(event chat.Event, request harness.RunRequest, session chat.Session) (bool, error) {
	switch event.Type {
	case "completion.accepted":
		if payload, ok := event.Data["payload"].(json.RawMessage); ok {
			c.out.Payload = append([]byte(nil), payload...)
		}
	case "error":
		if fatal, _ := event.Data["fatal"].(bool); fatal {
			return false, fmt.Errorf("chat driver reported execution failure")
		}
	case "turn_ended":
		if settled, known := event.Data["settled"].(bool); known {
			if !settled {
				return false, nil
			}
			c.queued = 0
		} else if c.queued > 0 {
			c.queued--
			return false, nil
		}
		if event.Data["reason"] != "end_turn" {
			return false, fmt.Errorf("chat turn did not complete")
		}
		return true, c.finish(request, session)
	}
	return false, nil
}
func (c *chatCompletion) finish(request harness.RunRequest, session chat.Session) error {
	if err := session.Close(); err != nil {
		return err
	}
	if len(c.out.Payload) == 0 {
		payload, err := readChatCompletion(request.Workspace, request.CompletionPath)
		if err != nil {
			return err
		}
		c.out.Payload = payload
	}
	if request.ValidateCompletion != nil {
		return request.ValidateCompletion(c.out.Payload)
	}
	return nil
}
