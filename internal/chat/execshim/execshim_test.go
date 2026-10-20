package execshim

import (
	"context"
	"errors"
	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/chat/chattest"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"testing"
	"time"
)

func TestConformance(t *testing.T) {
	chattest.Conformance(t, func() chat.Driver {
		return &Driver{Run: func(context.Context, harness.RunRequest) (harness.Outcome, error) { return harness.Outcome{}, nil }}
	})
}
func TestInterruptJoinsBeforeContinuation(t *testing.T) {
	started := make(chan struct{})
	joined := make(chan struct{})
	calls := 0
	d := &Driver{Run: func(ctx context.Context, _ harness.RunRequest) (harness.Outcome, error) {
		calls++
		ack := invoke.RegisterWorkspaceWriter(ctx)
		defer ack(nil)
		if calls == 1 {
			close(started)
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond)
			close(joined)
			return harness.Outcome{}, ctx.Err()
		}
		return harness.Outcome{Payload: []byte(`{"status":"success"}`)}, nil
	}}
	s, _ := d.Start(context.Background(), chat.StartOptions{})
	defer s.Close()
	_, _ = s.Send(context.Background(), chat.Text("first"), chat.SendOptions{Delivery: chat.Prompt})
	<-started
	if _, err := s.Send(context.Background(), chat.Text("steer"), chat.SendOptions{Delivery: chat.Steer}); !errors.Is(err, chat.ErrUnsupported) {
		t.Fatal(err)
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("interrupt returned before join")
	}
	if delivery, err := s.Send(context.Background(), chat.Text("second"), chat.SendOptions{Delivery: chat.Prompt}); err != nil || delivery != chat.Prompt {
		t.Fatal(delivery, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestUnacknowledgedWriterRetainsCustody(t *testing.T) {
	d := &Driver{Run: func(ctx context.Context, _ harness.RunRequest) (harness.Outcome, error) {
		_ = invoke.RegisterWorkspaceWriter(ctx)
		return harness.Outcome{Payload: []byte(`{}`)}, nil
	}}
	s, _ := d.Start(context.Background(), chat.StartOptions{})
	_, _ = s.Send(context.Background(), chat.Text("first"), chat.SendOptions{Delivery: chat.Prompt})
	if err := s.Close(); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal(err)
	}
}
