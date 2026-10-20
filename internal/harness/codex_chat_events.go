package harness

// Event normalization follows Clubhouse's codex-chat-events.ts, commit
// 3309665c (MIT). See internal/codexprotocol/CLUBHOUSE-LICENSE.
import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/chat"
	cp "github.com/goobers/goobers/internal/codexprotocol"
)

type codexChatQuestion struct {
	id        json.RawMessage
	turn      string
	questions []cp.ToolRequestUserInputQuestion
}

func (s *codexChatSession) unknown(kind, name string) {
	s.emit("protocol_unknown", map[string]any{"driver": "codex_app_server", "kind": kind, "name": boundedCodexText(name)}, "")
}

func (s *codexChatSession) reply(id json.RawMessage, result any, rpcErr *codexRPCError) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg := codexRPCMessage{ID: id, Error: rpcErr}
	if rpcErr == nil {
		msg.Result = codexRaw(result)
	}
	err := s.rpc.send(ctx, msg)
	if err != nil {
		s.rpc.fail(err)
	}
	return err
}

func (s *codexChatSession) receive(msg codexRPCMessage) {
	if s.closing.Load() {
		return
	}
	var routing struct {
		ThreadID       string `json:"threadId"`
		ConversationID string `json:"conversationId"`
	}
	_ = json.Unmarshal(msg.Params, &routing)
	if routing.ThreadID == "" {
		routing.ThreadID = routing.ConversationID
	}
	s.mu.Lock()
	id := s.nativeID
	if id == "" && len(msg.ID) == 0 && routing.ThreadID != "" {
		if len(s.early) >= 128 {
			s.mu.Unlock()
			s.rpc.fail(fmt.Errorf("codex chat startup event limit exceeded"))
			return
		}
		s.early = append(s.early, msg)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if routing.ThreadID != "" && id != "" && routing.ThreadID != id {
		if len(msg.ID) > 0 {
			_ = s.reply(msg.ID, nil, &codexRPCError{Code: -32602, Message: "request belongs to another thread"})
		}
		return
	}
	if len(msg.ID) > 0 {
		s.serverRequest(msg)
		return
	}
	switch msg.Method {
	case "turn/started":
		s.turnStarted(msg)
	case "turn/completed":
		s.turnCompleted(msg)
	case "error":
		s.turnError(msg)
	case "item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		s.messageDelta(msg)
	case "item/started", "item/completed":
		s.item(msg)
	case "thread/tokenUsage/updated":
		s.usageUpdated(msg)
	case "turn/plan/updated":
		s.planUpdated(msg)
	case "mcpServer/startupStatus/updated":
		s.mcpStartup(msg)
	case "thread/started", "thread/status/changed": // Lifecycle is emitted after the start response.
	default:
		s.unknown("notification", msg.Method)
	}
}

func (s *codexChatSession) turnStarted(msg codexRPCMessage) {
	var p cp.TurnStartedNotification
	if json.Unmarshal(msg.Params, &p) != nil || p.Turn.Id == "" {
		s.unknown("payload", msg.Method)
		return
	}
	if s.activateTurn(p.Turn.Id) {
		s.emit("turn_started", map[string]any{"nativeTurnId": p.Turn.Id}, "")
	}
}

func (s *codexChatSession) activateTurn(id string) bool {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || s.active == id || s.lastEnded == id {
		return false
	}
	s.active = id
	return true
}

func (s *codexChatSession) turnCompleted(msg codexRPCMessage) {
	var p cp.TurnCompletedNotification
	if json.Unmarshal(msg.Params, &p) != nil || p.Turn.Id == "" {
		s.unknown("payload", msg.Method)
		return
	}
	s.cancelQuestions(p.Turn.Id)
	s.mu.Lock()
	if s.lastEnded == p.Turn.Id {
		s.mu.Unlock()
		return
	}
	s.lastEnded = p.Turn.Id
	if s.active == p.Turn.Id {
		s.active = ""
	}
	reported := s.erroredTurn == p.Turn.Id
	if p.Turn.Status == "failed" {
		s.erroredTurn = p.Turn.Id
	}
	s.mu.Unlock()
	reason := "end_turn"
	if p.Turn.Status == "interrupted" {
		reason = "cancelled"
	}
	if p.Turn.Status == "failed" {
		reason = "error"
		if !reported {
			s.emit("error", map[string]any{"code": "turn_failed", "message": "Codex turn failed", "fatal": false}, "")
		}
	}
	s.emit("turn_ended", map[string]any{"nativeTurnId": p.Turn.Id, "reason": reason}, "")
}

func (s *codexChatSession) turnError(msg codexRPCMessage) {
	var p cp.ErrorNotification
	if json.Unmarshal(msg.Params, &p) != nil || p.TurnId == "" {
		s.unknown("payload", msg.Method)
		return
	}
	if !p.WillRetry {
		s.cancelQuestions(p.TurnId)
	}
	s.mu.Lock()
	duplicate := !p.WillRetry && s.erroredTurn == p.TurnId
	if !p.WillRetry {
		s.erroredTurn = p.TurnId
	}
	s.mu.Unlock()
	if !duplicate {
		s.emit("error", map[string]any{"code": "turn_error", "message": "Codex reported a turn error", "retrying": p.WillRetry, "fatal": false}, "")
	}
}

func (s *codexChatSession) messageDelta(msg codexRPCMessage) {
	var p cp.AgentMessageDeltaNotification
	if json.Unmarshal(msg.Params, &p) != nil {
		s.unknown("payload", msg.Method)
		return
	}
	kind := "agent_message_chunk"
	if msg.Method != "item/agentMessage/delta" {
		kind = "agent_thought_chunk"
	}
	s.emit(kind, map[string]any{"messageId": p.ItemId, "content": chat.Text(p.Delta)}, "")
}

func (s *codexChatSession) usageUpdated(msg codexRPCMessage) {
	var p cp.ThreadTokenUsageUpdatedNotification
	if json.Unmarshal(msg.Params, &p) != nil {
		s.unknown("payload", msg.Method)
		return
	}
	s.mu.Lock()
	in, out := p.TokenUsage.Last.InputTokens, p.TokenUsage.Last.OutputTokens
	if s.usageKnown && p.TokenUsage.Total.InputTokens >= s.usage.InputTokens && p.TokenUsage.Total.OutputTokens >= s.usage.OutputTokens {
		in = p.TokenUsage.Total.InputTokens - s.usage.InputTokens
		out = p.TokenUsage.Total.OutputTokens - s.usage.OutputTokens
	}
	s.usage = p.TokenUsage.Total
	s.usageKnown = true
	s.mu.Unlock()
	if in > 0 || out > 0 {
		s.emit("usage", map[string]any{"inputTokens": max(int64(0), in), "outputTokens": max(int64(0), out)}, "")
	}
}

func (s *codexChatSession) planUpdated(msg codexRPCMessage) {
	var p struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if json.Unmarshal(msg.Params, &p) != nil {
		s.unknown("payload", msg.Method)
		return
	}
	s.emit("plan", map[string]any{"steps": p.Plan}, "")
}

func (s *codexChatSession) mcpStartup(msg codexRPCMessage) {
	var p struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if json.Unmarshal(msg.Params, &p) == nil && p.Status == "failed" && s.prepared.mcp[p.Name] != nil {
		s.emit("error", map[string]any{"code": "required_mcp_failed", "message": "A required MCP server failed to start", "fatal": true}, "")
		s.rpc.fail(fmt.Errorf("required MCP server failed"))
	}
}

func (s *codexChatSession) serverRequest(msg codexRPCMessage) {
	switch msg.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "execCommandApproval", "applyPatchApproval":
		// No acceptance path: these requests may carry unsandboxed escalation,
		// network grants or persistent exec-policy amendments. Never echo proposals.
		result := map[string]any{"decision": "decline"}
		if msg.Method == "item/permissions/requestApproval" {
			result = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
		}
		if msg.Method == "execCommandApproval" || msg.Method == "applyPatchApproval" {
			result = map[string]any{"decision": "denied"}
		}
		s.emit("notice", map[string]any{"code": "approval_declined", "message": "Codex requested additional permission; this driver keeps the existing sandbox and declined the request.", "method": msg.Method}, "")
		_ = s.reply(msg.ID, result, nil)
	case "item/tool/requestUserInput":
		s.questionRequest(msg)
	default:
		s.unknown("request", msg.Method)
		_ = s.reply(msg.ID, nil, &codexRPCError{Code: -32601, Message: "method not supported"})
	}
}

func (s *codexChatSession) questionRequest(msg codexRPCMessage) {
	var p cp.ToolRequestUserInputParams
	if json.Unmarshal(msg.Params, &p) != nil || len(p.Questions) == 0 || len(p.Questions) > 32 {
		_ = s.reply(msg.ID, nil, &codexRPCError{Code: -32602, Message: "invalid questions"})
		return
	}
	ids := map[string]bool{}
	questions := make([]map[string]any, 0, len(p.Questions))
	for _, q := range p.Questions {
		if q.IsSecret || q.Id == "" || ids[q.Id] {
			_ = s.reply(msg.ID, nil, &codexRPCError{Code: -32602, Message: "secret or invalid questions are unsupported"})
			return
		}
		ids[q.Id] = true
		questions = append(questions, map[string]any{"id": q.Id, "header": q.Header, "question": q.Question, "options": q.Options, "allowOther": q.IsOther})
	}
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	s.mu.Lock()
	if s.closing.Load() || p.TurnId == "" || p.TurnId != s.active || p.TurnId == s.blockedTurn {
		s.mu.Unlock()
		_ = s.reply(msg.ID, map[string]any{"answers": map[string]any{}}, nil)
		return
	}
	for _, q := range s.questions {
		if string(q.id) == string(msg.ID) {
			s.mu.Unlock()
			_ = s.reply(msg.ID, nil, &codexRPCError{Code: -32600, Message: "duplicate question request"})
			return
		}
	}
	if len(s.questions) >= 64 {
		s.mu.Unlock()
		_ = s.reply(msg.ID, nil, &codexRPCError{Code: -32600, Message: "excessive question requests"})
		return
	}
	// Host IDs never repeat, even if the server reuses its RPC ID next turn.
	s.questionSeq++
	key := fmt.Sprintf("codex-question-%d", s.questionSeq)
	s.questions[key] = codexChatQuestion{id: msg.ID, turn: p.TurnId, questions: p.Questions}
	s.mu.Unlock()
	s.emit("question", map[string]any{"requestId": key, "questions": questions}, "")
}

func (s *codexChatSession) RespondPermission(context.Context, string, chat.PermissionResponse) error {
	if s.closing.Load() {
		return chat.ErrClosed
	}
	return chat.ErrUnknownRequest
}
func (s *codexChatSession) AnswerQuestion(ctx context.Context, id string, answers chat.QuestionAnswers) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closing.Load() {
		return chat.ErrClosed
	}
	s.mu.Lock()
	q, ok := s.questions[id]
	if !ok || q.turn != s.active || q.turn == s.blockedTurn {
		s.mu.Unlock()
		return chat.ErrUnknownRequest
	}
	if len(answers) != len(q.questions) {
		s.mu.Unlock()
		return fmt.Errorf("answer every question exactly once")
	}
	result := map[string]any{}
	for _, question := range q.questions {
		answer, exists := answers[question.Id]
		if !exists || len(answer) == 0 {
			s.mu.Unlock()
			return fmt.Errorf("missing question answer")
		}
		result[question.Id] = map[string]any{"answers": answer}
	}
	delete(s.questions, id)
	s.mu.Unlock()
	if err := s.reply(q.id, map[string]any{"answers": result}, nil); err != nil {
		return err
	}
	s.emit("question_resolved", map[string]any{"requestId": id, "resolution": "answered"}, "")
	return nil
}
func (s *codexChatSession) cancelQuestions(turn string) {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	s.mu.Lock()
	if turn != "" && turn == s.active {
		s.blockedTurn = turn
	}
	pending := map[string]codexChatQuestion{}
	for id, q := range s.questions {
		if turn == "" || q.turn == turn {
			pending[id] = q
			delete(s.questions, id)
		}
	}
	s.mu.Unlock()
	for id, q := range pending {
		if !s.closing.Load() {
			_ = s.reply(q.id, map[string]any{"answers": map[string]any{}}, nil)
		}
		s.emit("question_resolved", map[string]any{"requestId": id, "resolution": "cancelled"}, "")
	}
}
func (s *codexChatSession) item(msg codexRPCMessage) {
	var p struct {
		Item struct {
			Type    string  `json:"type"`
			ID      string  `json:"id"`
			Text    string  `json:"text"`
			Command string  `json:"command"`
			Name    string  `json:"tool"`
			Status  string  `json:"status"`
			Output  *string `json:"aggregatedOutput"`
		} `json:"item"`
	}
	if json.Unmarshal(msg.Params, &p) != nil || p.Item.ID == "" {
		s.unknown("payload", msg.Method)
		return
	}
	i := p.Item
	complete := msg.Method == "item/completed"
	switch i.Type {
	case "userMessage":
		return
	case "agentMessage":
		if complete {
			s.emit("agent_message", map[string]any{"messageId": i.ID, "content": chat.Text(i.Text)}, "")
		}
		return
	case "reasoning":
		return // Text deltas carry the normalized thought stream.
	}
	kind, title := "other", ""
	switch i.Type {
	case "commandExecution":
		kind, title = "execute", i.Command
	case "fileChange":
		kind, title = "edit", "File changes"
	case "mcpToolCall":
		title = i.Name
	case "webSearch":
		kind, title = "read", "Web search"
	default:
		s.unknown("item", i.Type)
		return
	}
	status := "in_progress"
	event := "tool_call"
	if complete {
		event = "tool_call_update"
		status = "completed"
		if i.Status == "failed" || i.Status == "declined" {
			status = "failed"
		}
	}
	data := map[string]any{"toolCallId": i.ID, "title": boundedCodexText(title), "kind": kind, "status": status}
	if complete && i.Output != nil {
		data["content"] = chat.Text(*i.Output)
	}
	s.emit(event, data, "")
}
