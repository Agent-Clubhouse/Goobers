package copilotsdk

import (
	"testing"

	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/chat"
)

func TestBufferedIdleRetainsNativeTurnIdentity(t *testing.T) {
	f := &fakeNative{}
	s, err := testDriver(f).Start(t.Context(), options())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	identity := s.(chat.ActiveTurnIdentifier)
	if _, err := s.Send(t.Context(), chat.Text("first"), chat.SendOptions{Delivery: chat.Prompt}); err != nil {
		t.Fatal(err)
	}
	f.emit(&rpc.AssistantTurnStartData{})
	first := identity.ActiveTurnID()
	f.emit(&rpc.SessionIdleData{})
	if _, err := s.Send(t.Context(), chat.Text("second"), chat.SendOptions{Delivery: chat.Steer}); err != nil {
		t.Fatal(err)
	}
	f.emit(&rpc.AssistantTurnStartData{})
	second := identity.ActiveTurnID()
	if first == "" || second == "" || first == second {
		t.Fatalf("native identities first=%q second=%q", first, second)
	}
	old := next(t, s, "turn_ended")
	if old.Data["nativeTurnId"] != first || identity.ActiveTurnID() != second {
		t.Fatalf("buffered idle lost its identity: %+v active=%s", old, identity.ActiveTurnID())
	}
	if err := s.Interrupt(t.Context()); err != nil {
		t.Fatal(err)
	}
	current := next(t, s, "turn_ended")
	if current.Data["nativeTurnId"] != second || current.Data["settled"] != true || identity.ActiveTurnID() != "" {
		t.Fatalf("current turn did not settle: %+v active=%s", current, identity.ActiveTurnID())
	}
}
