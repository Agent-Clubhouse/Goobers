package chattest

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/chat"
)

// Conformance runs common lifecycle and ceiling assertions. Factories must use
// fixture-backed transports, never a live model in CI. Provider tests exercise
// native turn, permission and usage translation separately.
func Conformance(t *testing.T, factory func() chat.Driver) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := factory()
	if d.ID() == "" {
		t.Fatal("empty driver ID")
	}
	p, err := d.Probe(ctx)
	if err != nil || !p.HandshakeOK {
		t.Fatalf("probe: %v", err)
	}
	s, err := d.Start(ctx, chat.StartOptions{AgentID: "agent", SessionID: "session", Fence: 7, Ceiling: chat.NewCeiling(chat.Ask, chat.Auto, chat.FullAccess)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	level, err := s.SetLevel(ctx, chat.FullAccess)
	if err == nil && level != chat.Ask {
		t.Fatalf("ceiling escaped: %s", level)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal("close not idempotent")
	}
	if _, err := s.Send(ctx, chat.Text("late"), chat.SendOptions{Delivery: chat.Prompt}); err == nil {
		t.Fatal("send after close succeeded")
	}
	var seq uint64
	for e := range s.Events() {
		if e.V != 2 || e.Seq <= seq || e.Fence != 7 || e.SessionID != "session" {
			t.Fatalf("bad envelope: %+v", e)
		}
		seq = e.Seq
	}
}
