package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/chat"
)

type codexChatCapture struct {
	sync.Mutex
	bytes.Buffer
}

func (c *codexChatCapture) Write(b []byte) (int, error) {
	c.Lock()
	defer c.Unlock()
	return c.Buffer.Write(b)
}
func (*codexChatCapture) Close() error { return nil }
func (c *codexChatCapture) messages(t *testing.T) []codexRPCMessage {
	t.Helper()
	c.Lock()
	defer c.Unlock()
	var result []codexRPCMessage
	for _, line := range bytes.Split(bytes.TrimSpace(c.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m codexRPCMessage
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		result = append(result, m)
	}
	return result
}
func codexChatEventFixture(t *testing.T) (*codexChatSession, *codexChatCapture) {
	t.Helper()
	c := &codexChatCapture{}
	s := &codexChatSession{nativeID: "thread", stream: chat.NewStream(chat.StartOptions{}), questions: map[string]codexChatQuestion{}}
	s.rpc = newCodexRPC(c, s.receive, nil)
	go s.rpc.writeLoop()
	t.Cleanup(func() { s.rpc.fail(chat.ErrClosed); s.stream.Close() })
	return s, c
}
func codexChatDrain(s *codexChatSession) []chat.Event {
	var events []chat.Event
	for {
		select {
		case e := <-s.Events():
			events = append(events, e)
		default:
			return events
		}
	}
}
func TestCodexChatServerRequestsAlwaysAnsweredWithinCeiling(t *testing.T) {
	s, c := codexChatEventFixture(t)
	methods := []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "execCommandApproval", "applyPatchApproval", "unknown/newRequest", "account/chatgptAuthTokens/refresh"}
	for _, method := range methods {
		s.receive(codexRPCMessage{ID: codexRaw(method), Method: method, Params: codexRaw(map[string]any{"threadId": "thread", "proposedExecpolicyAmendment": []string{"allow-all"}, "secret": "must-not-leak"})})
	}
	messages := c.messages(t)
	if len(messages) != len(methods) {
		t.Fatalf("unanswered requests: %d", len(messages))
	}
	for i, m := range messages {
		if string(m.ID) != string(codexRaw(methods[i])) {
			t.Fatal("request ID changed")
		}
		if i >= 5 && (m.Error == nil || m.Error.Code != -32601) {
			t.Fatalf("unknown request not rejected: %+v", m)
		}
		if strings.Contains(string(m.Result), "accept") || strings.Contains(string(m.Result), "allow-all") {
			t.Fatal("permission expanded")
		}
	}
	events := codexChatDrain(s)
	data, _ := json.Marshal(events)
	if strings.Contains(string(data), "must-not-leak") || strings.Contains(string(data), "allow-all") {
		t.Fatal("raw RPC data leaked")
	}
	if err := s.RespondPermission(context.Background(), string(codexRaw(methods[0])), chat.PermissionResponse{OptionID: "accept"}); !errors.Is(err, chat.ErrUnknownRequest) {
		t.Fatal("human acceptance path exists")
	}
}
func TestCodexChatQuestionsAndThreadFiltering(t *testing.T) {
	s, c := codexChatEventFixture(t)
	params := map[string]any{"threadId": "thread", "turnId": "turn", "questions": []any{map[string]any{"id": "q", "header": "Choice", "question": "Choose", "options": []any{map[string]any{"label": "One", "description": "first"}}}}}
	s.receive(codexRPCMessage{ID: json.RawMessage(`7`), Method: "item/tool/requestUserInput", Params: codexRaw(params)})
	if len(c.messages(t)) != 0 {
		t.Fatal("question answered before user")
	}
	if err := s.AnswerQuestion(context.Background(), "7", chat.QuestionAnswers{"q": []string{"One"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerQuestion(context.Background(), "7", chat.QuestionAnswers{"q": []string{"One"}}); !errors.Is(err, chat.ErrUnknownRequest) {
		t.Fatal("duplicate answer accepted")
	}
	params["threadId"] = "other"
	s.receive(codexRPCMessage{ID: json.RawMessage(`"foreign"`), Method: "item/tool/requestUserInput", Params: codexRaw(params)})
	messages := c.messages(t)
	if len(messages) != 2 || messages[1].Error == nil {
		t.Fatal("foreign thread request not rejected")
	}
	s.receive(codexRPCMessage{Method: "item/agentMessage/delta", Params: codexRaw(map[string]any{"threadId": "other", "delta": "foreign message"})})
	for _, e := range codexChatDrain(s) {
		if e.Type == "agent_message_chunk" {
			t.Fatal("foreign thread leaked")
		}
	}
}
func TestCodexChatFailedTurnsAndUsageDeduplicate(t *testing.T) {
	s, _ := codexChatEventFixture(t)
	notify := func(method string, p any) { s.receive(codexRPCMessage{Method: method, Params: codexRaw(p)}) }
	notify("turn/started", map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "inProgress"}})
	notify("error", map[string]any{"threadId": "thread", "turnId": "turn", "willRetry": false, "error": map[string]any{"message": "failure"}})
	completed := map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "failed"}}
	notify("turn/completed", completed)
	notify("turn/completed", completed)
	usage := func(total, last int) {
		notify("thread/tokenUsage/updated", map[string]any{"threadId": "thread", "tokenUsage": map[string]any{"total": map[string]any{"inputTokens": total}, "last": map[string]any{"inputTokens": last}}})
	}
	usage(1000, 10)
	usage(1000, 10)
	usage(1015, 15)
	counts := map[string]int{}
	var tokens int64
	for _, e := range codexChatDrain(s) {
		counts[e.Type]++
		if e.Type == "usage" {
			tokens += e.Data["inputTokens"].(int64)
		}
	}
	if counts["error"] != 1 || counts["turn_ended"] != 1 || tokens != 25 {
		t.Fatalf("events=%v tokens=%d", counts, tokens)
	}
}
func TestCodexChatRPCMalformedAndCancellation(t *testing.T) {
	input := &codexChatCapture{}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	received := make(chan codexRPCMessage, 4)
	rpc := newCodexRPC(input, func(m codexRPCMessage) { received <- m }, nil)
	rpc.start(reader)
	defer rpc.fail(chat.ErrClosed)
	go func() { _, _ = io.WriteString(writer, "invalid\n{\"method\":\"valid\",\"params\":{}}\n") }()
	for _, want := range []string{"malformed_frame", "valid"} {
		select {
		case got := <-received:
			if got.Method != want {
				t.Fatal(got.Method)
			}
		case <-time.After(time.Second):
			t.Fatal("reader wedged")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := rpc.request(ctx, "turn/start", map[string]any{}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-rpc.done:
	default:
		t.Fatal("uncertain turn delivery left session alive")
	}
}
