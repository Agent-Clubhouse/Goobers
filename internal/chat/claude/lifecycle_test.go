package claude

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/chat"
)

func TestIdleInterruptDoesNotCancelFutureTurn(t *testing.T) {
	d, _ := fakeDriver(t, "controls")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := d.Start(ctx, chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if err := session.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Send(ctx, chat.Text("hold"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	session.(*Session).frame(object{"type": "result", "subtype": "success"})
	e := waitEvent(t, session, "turn_ended")
	if e.Data["reason"] != "end_turn" {
		t.Fatalf("successful next turn ended as %v after idle interrupt", e.Data)
	}
}

func TestTerminalResultInvalidatesPendingPermission(t *testing.T) {
	d, _ := fakeDriver(t, "controls")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := d.Start(ctx, chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if _, err := session.Send(ctx, chat.Text("permission"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, session, "permission_request")
	session.(*Session).frame(object{"type": "result", "subtype": "error_during_execution", "is_error": true})
	waitEvent(t, session, "turn_ended")
	if err := session.RespondPermission(ctx, "allow-1", chat.PermissionResponse{OptionID: "allow_once"}); !errors.Is(err, chat.ErrUnknownRequest) {
		t.Fatalf("terminal turn retained approvable permission: %v", err)
	}
}

func TestAcknowledgedInterruptInvalidatesPermission(t *testing.T) {
	d, _ := fakeDriver(t, "controls")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := d.Start(ctx, chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if _, err := session.Send(ctx, chat.Text("permission"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, session, "permission_request")
	if err := session.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if err := session.RespondPermission(ctx, "allow-1", chat.PermissionResponse{OptionID: "allow_once"}); !errors.Is(err, chat.ErrUnknownRequest) {
		t.Fatalf("interrupted turn retained approvable permission: %v", err)
	}
}
