package interactiveexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
)

type steeringDelivery struct {
	restart     *interactive.Steering
	queued      bool
	interrupted bool
}

func managementBridgeAttached(request harness.RunRequest) bool {
	for _, server := range request.MCPServers {
		if server.Name == "goobers-interactive" {
			return true
		}
	}
	return false
}

func deliverSteering(ctx context.Context, session chat.Session, steer interactive.Steering, request harness.RunRequest, project func(chat.Event) error, emit func(string, map[string]any) error) (steeringDelivery, error) {
	requested := chat.Delivery(steer.Delivery)
	if requested == "" {
		requested = chat.Steer
	}
	if requested == chat.Queued && session.Capabilities().Queue == "none" {
		acknowledge(steer, "", chat.ErrUnsupported)
		return steeringDelivery{}, nil
	}
	if steer.SteeringMode == "interrupt-and-continue" || requested == chat.Steer && !session.Capabilities().Steer {
		if managementBridgeAttached(request) {
			return restartAfterInterrupt(session, steer, emit, "bridge-attached: restart for custody")
		}
		return interruptAndContinue(ctx, session, steer, request, project, emit, 5*time.Second)
	}
	delivery, err := session.Send(ctx, chat.Text(steer.Text), chat.SendOptions{Delivery: requested, CommandID: steer.CommandID})
	acknowledge(steer, delivery, err)
	if errors.Is(err, chat.ErrClosed) {
		return steeringDelivery{}, err
	}
	return steeringDelivery{queued: err == nil && delivery == chat.Queued}, nil
}

func interruptAndContinue(ctx context.Context, session chat.Session, steer interactive.Steering, request harness.RunRequest, project func(chat.Event) error, emit func(string, map[string]any) error, timeout time.Duration) (steeringDelivery, error) {
	if !session.Capabilities().Interrupt {
		return restartAfterInterrupt(session, steer, emit, "unsupported")
	}
	interruptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := session.Interrupt(interruptCtx); err != nil {
		if ctx.Err() != nil {
			acknowledge(steer, "", ctx.Err())
			return steeringDelivery{}, ctx.Err()
		}
		return restartAfterInterrupt(session, steer, emit, interruptFailure(interruptCtx, err))
	}
	restart, err := awaitInterruptedIdle(interruptCtx, session, project)
	if err != nil {
		if !restart || ctx.Err() != nil {
			acknowledge(steer, "", err)
			return steeringDelivery{}, err
		}
		return restartAfterInterrupt(session, steer, emit, interruptFailure(interruptCtx, err))
	}
	if err := steeringPath(emit, steer, "native_interrupt", "settled"); err != nil {
		acknowledge(steer, "", err)
		return steeringDelivery{}, err
	}
	if err := clearInterruptedCompletion(request); err != nil {
		acknowledge(steer, "", err)
		return steeringDelivery{}, err
	}
	delivery, err := session.Send(ctx, chat.Text(steer.Text), chat.SendOptions{Delivery: chat.Prompt, CommandID: steer.CommandID})
	acknowledge(steer, delivery, err)
	return steeringDelivery{interrupted: true, queued: err == nil && delivery == chat.Queued}, err
}

func clearInterruptedCompletion(request harness.RunRequest) error {
	if request.CompletionPath == "" {
		return nil
	}
	root, err := os.OpenRoot(request.Workspace)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(request.CompletionPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func awaitInterruptedIdle(ctx context.Context, session chat.Session, project func(chat.Event) error) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case event, ok := <-session.Events():
			if !ok {
				return true, chat.ErrClosed
			}
			if err := project(event); err != nil {
				return false, err
			}
			if event.Type == "turn_ended" && event.Data["settled"] == true {
				return false, nil
			}
			if event.Type == "error" && event.Data["fatal"] == true {
				return true, fmt.Errorf("driver failed while interrupting")
			}
		}
	}
}

func interruptFailure(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "failed"
}

func restartAfterInterrupt(session chat.Session, steer interactive.Steering, emit func(string, map[string]any) error, reason string) (steeringDelivery, error) {
	if err := session.Close(); err != nil {
		acknowledge(steer, "", err)
		return steeringDelivery{}, err
	}
	if err := steeringPath(emit, steer, "restart", reason); err != nil {
		acknowledge(steer, "", err)
		return steeringDelivery{}, err
	}
	return steeringDelivery{restart: &steer}, nil
}

func steeringPath(emit func(string, map[string]any) error, steer interactive.Steering, path, reason string) error {
	return emit("execution.note", map[string]any{"commandId": steer.CommandID, "steeringPath": path, "reason": reason})
}
