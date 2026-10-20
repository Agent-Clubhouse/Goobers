package chat_test

import (
	"context"
	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/chat/chattest"
	"strings"
	"testing"
)

func TestFakeConformance(t *testing.T) {
	chattest.Conformance(t, func() chat.Driver { return &chattest.FakeDriver{Caps: chat.MinimalCapabilities()} })
}
func TestUnknownAndSequence(t *testing.T) {
	s := chat.NewStream(chat.StartOptions{SessionID: "s", LastSeq: 9, Fence: 4})
	s.Emit("new.native.event", map[string]any{"secret": "must not escape"}, "c")
	s.Close()
	e := <-s.Events()
	if e.Type != "protocol_unknown" || e.Data["secret"] != nil || e.Seq != 10 || e.CommandID != "c" {
		t.Fatalf("bad event %+v", e)
	}
}
func TestRegistryFallback(t *testing.T) {
	r := chat.NewRegistry()
	_ = r.Register("copilot", &chattest.FakeDriver{})
	s, attempts, err := r.Start(context.Background(), "copilot", chat.StartOptions{})
	if err != nil || len(attempts) != 1 {
		t.Fatal(err, attempts)
	}
	s.Close()
	if _, _, err := r.Start(context.Background(), "unknown", chat.StartOptions{}); err == nil {
		t.Fatal("cross-provider fallback")
	}
}
func TestFixtureRemapAndMismatch(t *testing.T) {
	f, err := chattest.ReadFixture(strings.NewReader(`{"kind":"header","format":"clubhouse-chat-fixture","version":1,"protocol":"copilot-sdk"}
{"from":"client","msg":{"id":1,"method":"session.send"},"match":{"params":{"mode":"immediate"}}}
{"from":"cli","msg":{"id":1,"result":{}}}
{"from":"cli","msg":{"id":1,"method":"permission.request"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	err = f.Replay(func() (map[string]any, error) {
		return map[string]any{"id": 42, "method": "session.send", "params": map[string]any{"mode": "immediate"}}, nil
	}, func(e chattest.Entry) error {
		n++
		if n == 1 && e.Msg["id"] != 42 {
			t.Fatal("ID not remapped")
		}
		if n == 2 && e.Msg["id"] != float64(1) {
			t.Fatal("server request remapped")
		}
		return nil
	})
	if err != nil || n != 2 {
		t.Fatal(err, n)
	}
	err = f.Replay(func() (map[string]any, error) {
		return map[string]any{"method": "session.send", "params": map[string]any{"mode": "enqueue", "secret": "hidden"}}, nil
	}, func(chattest.Entry) error { return nil })
	if err == nil || strings.Contains(err.Error(), "hidden") {
		t.Fatal("missing or unsafe mismatch")
	}
}
