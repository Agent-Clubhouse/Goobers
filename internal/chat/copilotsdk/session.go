package copilotsdk

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/chat"
)

type session struct {
	ctx                       context.Context
	cancel                    context.CancelFunc
	mu                        sync.Mutex
	sendMu                    sync.Mutex
	stream                    *chat.Stream
	backend                   *backend
	native                    nativeSession
	id                        string
	unsubscribe               func()
	ceiling                   chat.Ceiling
	level                     chat.Level
	busy, interrupted, closed bool
	fallback                  chat.ResumeFallback
	permissions               map[string]chan chat.PermissionResponse
	questions                 map[string]chan chat.QuestionAnswers
	nextRequest               uint64
	requestEpoch              uint64
	queued                    int
	sendPending               bool
	pendingIdle               []idleBoundary
	nativeTurn                uint64
	nativeIdle                bool
	once                      sync.Once
	closeErr                  error
}

func newSession(ctx context.Context, cancel context.CancelFunc, o chat.StartOptions) *session {
	return &session{ctx: ctx, cancel: cancel, stream: chat.NewStream(o), ceiling: o.Ceiling, level: o.Ceiling.Clamp(o.Level), permissions: map[string]chan chat.PermissionResponse{}, questions: map[string]chan chat.QuestionAnswers{}}
}

// NativeID returns the native conversation identity.
func (s *session) NativeID() string { return s.id }

// Capabilities advertises the operations implemented by a transport.
func (s *session) Capabilities() chat.Capabilities { return capabilities() }

// ResumeFallback identifies why a fresh session replaced a requested resume.
func (s *session) ResumeFallback() chat.ResumeFallback { return s.fallback }

// Events returns the ordered event channel.
func (s *session) Events() <-chan chat.Event { return s.stream.Events() }
func (s *session) emit(t string, data map[string]any, id string) {
	if !s.stream.Emit(t, data, id) {
		s.cancel()
	}
}
func (s *session) callContext(ctx context.Context) (context.Context, func()) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	stop := context.AfterFunc(s.ctx, cancel)
	return c, func() { stop(); cancel() }
}

// Send accepts content and reports its actual delivery kind.
func (s *session) Send(ctx context.Context, content []chat.ContentBlock, o chat.SendOptions) (chat.Delivery, error) {
	text, err := chat.PlainText(content)
	if err != nil {
		return "", err
	}
	if o.Delivery != "" && o.Delivery != chat.Prompt && o.Delivery != chat.Steer && o.Delivery != chat.Queued {
		return "", fmt.Errorf("copilot-sdk: invalid delivery")
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", chat.ErrClosed
	}
	wasBusy := s.busy
	actual := o.Delivery
	if !wasBusy {
		actual = chat.Prompt
	} else if actual == "" || actual == chat.Prompt {
		s.mu.Unlock()
		return "", chat.ErrBusy
	}
	if !wasBusy {
		s.busy = true
		s.interrupted = false
	}
	if actual == chat.Queued {
		s.queued++
	}
	s.sendPending = true
	s.mu.Unlock()
	defer s.flushPendingIdle()
	mode := rpc.SendModeImmediate
	if actual == chat.Queued {
		mode = rpc.SendModeEnqueue
	}
	s.emit("command.accepted", map[string]any{}, o.CommandID)
	call, stop := s.callContext(ctx)
	defer stop()
	_, err = s.native.Send(call, sdk.MessageOptions{Prompt: text, Mode: string(mode)})
	if err != nil {
		s.mu.Lock()
		if !wasBusy {
			s.busy = false
		}
		if actual == chat.Queued && s.queued > 0 {
			s.queued--
		}
		s.mu.Unlock()
		s.emit("command.rejected", map[string]any{"code": "send_failed"}, o.CommandID)
		return "", fmt.Errorf("copilot-sdk: send failed")
	}
	s.emit("user_message", map[string]any{"content": content, "source": "user", "delivery": actual}, o.CommandID)
	s.emit("command.delivered", map[string]any{"delivery": actual}, o.CommandID)
	return actual, nil
}

// Interrupt stops the current turn without granting new authority.
func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return chat.ErrClosed
	}
	s.interrupted = true
	s.cancelRequestsLocked()
	s.mu.Unlock()
	call, stop := s.callContext(ctx)
	defer stop()
	return s.native.Abort(call)
}

// SetLevel requests a permission level bounded by host authority.
func (s *session) SetLevel(_ context.Context, l chat.Level) (chat.Level, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.level, chat.ErrClosed
	}
	s.level = s.ceiling.Clamp(l)
	s.emit("mode_changed", map[string]any{"level": s.level}, "")
	return s.level, nil
}

// SetModel requests a model change when the transport supports it.
func (s *session) SetModel(ctx context.Context, m string) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return chat.ErrClosed
	}
	call, stop := s.callContext(ctx)
	defer stop()
	if err := s.native.SetModel(call, m, nil); err != nil {
		return fmt.Errorf("copilot-sdk: model change failed")
	}
	s.emit("model_changed", map[string]any{"modelId": m}, "")
	return nil
}

// Close closes the session or stream after releasing its owned resources.
func (s *session) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancelRequestsLocked()
		s.mu.Unlock()
		s.cancel()
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		if s.backend != nil {
			s.closeErr = s.backend.close()
		}
		if s.native != nil {
			_ = s.native.Disconnect()
		}
		if s.closeErr != nil {
			s.stream.Emit("custody.pending", map[string]any{}, "")
		} else {
			s.stream.Emit("custody.released", map[string]any{}, "")
		}
		s.stream.Emit("session_ended", map[string]any{}, "")
		s.stream.Close()
	})
	return s.closeErr
}

// RespondPermission answers a pending permission request by identity.
func (s *session) RespondPermission(_ context.Context, id string, r chat.PermissionResponse) error {
	s.mu.Lock()
	ch, ok := s.permissions[id]
	if ok {
		delete(s.permissions, id)
	}
	s.mu.Unlock()
	if !ok {
		return chat.ErrUnknownRequest
	}
	ch <- r
	return nil
}

// AnswerQuestion answers a pending structured question by identity.
func (s *session) AnswerQuestion(_ context.Context, id string, a chat.QuestionAnswers) error {
	s.mu.Lock()
	ch, ok := s.questions[id]
	if ok {
		delete(s.questions, id)
	}
	s.mu.Unlock()
	if !ok {
		return chat.ErrUnknownRequest
	}
	ch <- a
	return nil
}
func (s *session) requestID() string {
	s.nextRequest++
	return fmt.Sprintf("request-%d", s.nextRequest)
}
func (s *session) askPermission(epoch uint64, kind string) (chat.PermissionResponse, error) {
	s.mu.Lock()
	if s.closed || s.interrupted || epoch != s.requestEpoch {
		s.mu.Unlock()
		return chat.PermissionResponse{}, chat.ErrClosed
	}
	id := s.requestID()
	ch := make(chan chat.PermissionResponse, 1)
	s.permissions[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.permissions, id); s.mu.Unlock() }()
	s.emit("permission_request", map[string]any{"requestId": id, "kind": kind, "options": []map[string]string{{"optionId": "allow_once", "kind": "allow_once", "label": "Allow once"}, {"optionId": "reject_once", "kind": "reject_once", "label": "Deny"}}}, "")
	select {
	case response := <-ch:
		if !s.requestCurrent(epoch) {
			response = chat.PermissionResponse{OptionID: "reject_once"}
		}
		s.emit("permission_resolved", map[string]any{"requestId": id, "optionId": response.OptionID}, "")
		return response, nil
	case <-s.ctx.Done():
		return chat.PermissionResponse{}, chat.ErrClosed
	}
}
func (s *session) permission(req sdk.PermissionRequest, inv sdk.PermissionInvocation) (rpc.PermissionDecision, error) {
	deny := &rpc.PermissionDecisionUserNotAvailable{}
	s.mu.Lock()
	epoch := s.requestEpoch
	s.mu.Unlock()
	if req == nil || s.backend.policy == nil {
		return deny, nil
	}
	decision, err := s.backend.policy(req, inv)
	if err != nil {
		return deny, nil
	}
	if _, ok := decision.(*rpc.PermissionDecisionApproveOnce); !ok {
		return deny, nil
	}
	s.mu.Lock()
	level := s.level
	closed := s.closed
	s.mu.Unlock()
	if closed || !s.requestCurrent(epoch) {
		return deny, nil
	}
	read := req.Kind() == rpc.PermissionRequestKindRead
	write := req.Kind() == rpc.PermissionRequestKindWrite
	if level == chat.Plan && !read {
		return deny, nil
	}
	if level == chat.Plan || level == chat.Auto || level == chat.FullAccess || level == chat.AutoEdit && (read || write) {
		return &rpc.PermissionDecisionApproveOnce{}, nil
	}
	response, err := s.askPermission(epoch, string(req.Kind()))
	if err != nil || response.OptionID != "allow_once" {
		return deny, nil
	}
	// Recheck the immutable outer policy and a possibly reduced live level after
	// a human waits. A stale approval must not widen either boundary.
	s.mu.Lock()
	level = s.level
	closed = s.closed
	s.mu.Unlock()
	if closed || !s.requestCurrent(epoch) || level == chat.Plan && !read {
		return deny, nil
	}
	decision, err = s.backend.policy(req, inv)
	if !s.requestCurrent(epoch) {
		return deny, nil
	}
	return decision, err
}
func (s *session) question(req sdk.UserInputRequest, _ sdk.UserInputInvocation) (sdk.UserInputResponse, error) {
	s.mu.Lock()
	if s.closed || s.interrupted {
		s.mu.Unlock()
		return sdk.UserInputResponse{}, chat.ErrClosed
	}
	epoch := s.requestEpoch
	id := s.requestID()
	ch := make(chan chat.QuestionAnswers, 1)
	s.questions[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.questions, id); s.mu.Unlock() }()
	s.emit("question", map[string]any{"requestId": id, "questions": []map[string]any{{"id": "answer", "question": req.Question, "options": req.Choices, "allowFreeform": req.AllowFreeform}}}, "")
	select {
	case a := <-ch:
		if a == nil || !s.requestCurrent(epoch) {
			s.emit("question_resolved", map[string]any{"requestId": id, "resolution": "cancelled"}, "")
			return sdk.UserInputResponse{}, fmt.Errorf("question cancelled by host policy")
		}
		answer := strings.Join(a["answer"], ", ")
		free := !slices.Contains(req.Choices, answer)
		if free && req.AllowFreeform != nil && !*req.AllowFreeform {
			return sdk.UserInputResponse{}, fmt.Errorf("answer is not an offered choice")
		}
		s.emit("question_resolved", map[string]any{"requestId": id}, "")
		return sdk.UserInputResponse{Answer: answer, WasFreeform: free}, nil
	case <-s.ctx.Done():
		return sdk.UserInputResponse{}, chat.ErrClosed
	}
}
func (s *session) plan(req sdk.ExitPlanModeRequest, _ sdk.ExitPlanModeInvocation) (sdk.ExitPlanModeResult, error) {
	s.mu.Lock()
	epoch := s.requestEpoch
	s.mu.Unlock()
	s.emit("plan_document", map[string]any{"content": req.PlanContent, "summary": req.Summary}, "")
	response, err := s.askPermission(epoch, "exit_plan_mode")
	if err != nil {
		return sdk.ExitPlanModeResult{}, err
	}
	approved := s.requestCurrent(epoch) && response.OptionID == "allow_once" && s.ceiling.Clamp(chat.AutoEdit) != chat.Plan
	action := ""
	if approved && slices.Contains(req.Actions, req.RecommendedAction) {
		action = req.RecommendedAction
	}
	return sdk.ExitPlanModeResult{Approved: approved, SelectedAction: action, Feedback: response.Message}, nil
}

func (s *session) requestCurrent(epoch uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && !s.interrupted && epoch == s.requestEpoch
}

// Called with mu held. Advance identity even when an answer was already removed
// from the map: its callback must still fail a post-interrupt policy recheck.
func (s *session) cancelRequestsLocked() {
	s.requestEpoch++
	for id, ch := range s.permissions {
		delete(s.permissions, id)
		ch <- chat.PermissionResponse{OptionID: "reject_once"}
	}
	for id, ch := range s.questions {
		delete(s.questions, id)
		ch <- nil
	}
}
