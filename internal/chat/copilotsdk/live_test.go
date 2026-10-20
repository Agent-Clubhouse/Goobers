package copilotsdk

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/harness"
)

// Explicit opt-in only. This spends real model tokens; ordinary CI skips it.
func TestLiveCopilotChat(t *testing.T) {
	if os.Getenv("GOOBERS_LIVE_CHAT") != "1" {
		t.Skip("live model opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	workspace := t.TempDir()
	d := &Driver{Adapter: &harness.CopilotAdapter{Command: []string{"copilot"}}, Request: harness.RunRequest{Mode: harness.ModeInvoke, Workspace: workspace, Tools: []string{"view"}, Timeout: 90 * time.Second}}
	if path := os.Getenv("GOOBERS_CHAT_FIXTURE"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = file.Close() }()
		encoder := json.NewEncoder(file)
		_ = encoder.Encode(map[string]any{"kind": "header", "format": "clubhouse-chat-fixture", "version": 1, "protocol": "copilot-sdk", "source": "recorded", "cli": map[string]string{"name": "copilot", "version": "1.0.98"}, "description": "Two live tiny turns; only allowlisted public content and usage retained; message IDs normalized"})
		original := *d
		var recordMu sync.Mutex
		d.open = func(ctx context.Context) (*backend, error) {
			b, err := original.connect(ctx)
			if err != nil {
				return nil, err
			}
			create := b.create
			b.create = func(ctx context.Context, c *sdk.SessionConfig) (nativeSession, string, error) {
				n, id, err := create(ctx, c)
				if err != nil {
					return nil, "", err
				}
				return recordingNative{nativeSession: n, record: func(e sdk.SessionEvent) {
					var data map[string]any
					switch v := e.Data.(type) {
					case *rpc.AssistantTurnStartData, *rpc.AssistantTurnEndData, *rpc.SessionIdleData:
						data = map[string]any{}
					case *rpc.AssistantMessageData:
						if v.Content != "ok" {
							return
						}
						data = map[string]any{"messageId": "message", "content": v.Content}
					case *rpc.AssistantMessageDeltaData:
						if v.DeltaContent != "ok" {
							return
						}
						data = map[string]any{"messageId": "message", "deltaContent": v.DeltaContent}
					case *rpc.AssistantUsageData:
						data = map[string]any{"inputTokens": v.InputTokens, "outputTokens": v.OutputTokens}
					default:
						return
					}
					recordMu.Lock()
					defer recordMu.Unlock()
					_ = encoder.Encode(map[string]any{"from": "cli", "msg": map[string]any{"type": e.Type(), "data": data}})
				}}, id, nil
			}
			return b, nil
		}
	}
	o := options()
	o.Workspace = workspace
	o.Level = chat.Plan
	o.SystemPrompt = "Answer the user's tiny test prompt without using tools."
	s, err := d.Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for i := 0; i < 2; i++ {
		actual, err := s.Send(ctx, chat.Text("Reply with only the word ok."), chat.SendOptions{Delivery: chat.Prompt})
		if err != nil || actual != chat.Prompt {
			t.Fatal(actual, err)
		}
		idle := false
		for !idle {
			select {
			case e := <-s.Events():
				t.Log(e.Type)
				if e.Type == "error" {
					t.Fatal(e.Data)
				}
				if e.Type == "agent_message" {
					t.Logf("reply: %v", e.Data["content"])
				}
				idle = e.Type == "turn_ended"
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

type recordingNative struct {
	nativeSession
	record func(sdk.SessionEvent)
}

func (r recordingNative) On(handler sdk.SessionEventHandler) func() {
	return r.nativeSession.On(func(e sdk.SessionEvent) { r.record(e); handler(e) })
}
