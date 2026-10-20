package execshim

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
)

func autoOptions() chat.StartOptions {
	return chat.StartOptions{Level: chat.Auto, Ceiling: chat.NewCeiling(chat.Auto, chat.Auto, chat.Auto)}
}
func TestPlanCeilingRefusesBeforeWriterStarts(t *testing.T) {
	called := false
	d := &Driver{Run: func(context.Context, harness.RunRequest) (harness.Outcome, error) {
		called = true
		return harness.Outcome{}, nil
	}}
	for _, level := range []chat.Level{chat.Plan, chat.Ask, chat.AutoEdit, chat.Auto, chat.FullAccess} {
		s, err := d.Start(t.Context(), chat.StartOptions{Level: level, Ceiling: chat.NewCeiling(chat.Plan, chat.FullAccess, chat.FullAccess)})
		if s != nil || !errors.Is(err, chat.ErrUnsupported) || called {
			t.Fatal("planning ceiling permitted an unrestricted writer", err)
		}
	}
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
	s, _ := d.Start(context.Background(), autoOptions())
	defer func() { _ = s.Close() }()
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
	s, _ := d.Start(context.Background(), autoOptions())
	_, _ = s.Send(context.Background(), chat.Text("first"), chat.SendOptions{Delivery: chat.Prompt})
	if err := s.Close(); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal(err)
	}
}

func TestInterruptDeadlineDoesNotReleaseWriterCustody(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	driver := &Driver{Run: func(ctx context.Context, _ harness.RunRequest) (harness.Outcome, error) {
		ack := invoke.RegisterWorkspaceWriter(ctx)
		defer ack(nil)
		close(started)
		<-release
		return harness.Outcome{}, ctx.Err()
	}}
	session, err := driver.Start(t.Context(), autoOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Send(t.Context(), chat.Text("first"), chat.SendOptions{Delivery: chat.Prompt}); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := session.Interrupt(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("interrupt did not honor deadline: %v", err)
	}
	if _, err := session.Send(t.Context(), chat.Text("second"), chat.SendOptions{Delivery: chat.Prompt}); !errors.Is(err, chat.ErrBusy) {
		t.Fatalf("writer admitted before quiescence: %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before writer quiesced: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	unblock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
