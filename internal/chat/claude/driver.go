package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
)

// Launch is supplied by the trusted harness after credentials, MCP and sandbox
// policy are applied. Prompt content must never be included in Command.
type Launch struct {
	Command, Env []string
	Dir          string
	Cleanup      func() error
}

// Driver requires a hardened launcher; it never inherits the host environment.
// Prepare also owns any per-session resources until Launch.Cleanup is called.
type Driver struct {
	Prepare        func(context.Context, chat.StartOptions, []string) (Launch, error)
	ProbeOptions   chat.StartOptions
	ControlTimeout time.Duration
}

// ID returns the stable native transport identity.
func (d *Driver) ID() string { return "claude_stream_json" }
func capabilities() chat.Capabilities {
	c := chat.MinimalCapabilities()
	c.PermissionExtras.DenyMessage = true
	c.Steer, c.Interrupt, c.Questions = true, true, true
	c.Queue, c.Resume, c.Subagents, c.Usage = "native", "native", "ids_only", "cost"
	c.ModelSwitch, c.EffortSwitch = "live", "live"
	c.LiveLevels = []chat.Level{chat.Plan, chat.Ask, chat.AutoEdit, chat.Auto, chat.FullAccess}
	c.SystemPrompt, c.Limits = true, true
	return c
}
func permissionMode(level chat.Level) string {
	switch level {
	case chat.Ask:
		return "default"
	case chat.AutoEdit:
		return "acceptEdits"
	case chat.Auto:
		return "auto"
	case chat.FullAccess:
		return "bypassPermissions"
	}
	return "plan"
}

func levelFromMode(mode string) (chat.Level, bool) {
	switch mode {
	case "plan":
		return chat.Plan, true
	case "default", "dontAsk":
		return chat.Ask, true
	case "acceptEdits":
		return chat.AutoEdit, true
	case "auto":
		return chat.Auto, true
	case "bypassPermissions":
		return chat.FullAccess, true
	}
	return "", false
}

// Args contains protocol metadata only. Even the system prompt travels through
// initialize, rather than an argv flag visible to other host processes.
func Args(o chat.StartOptions) []string {
	mode := permissionMode(o.Ceiling.Clamp(o.Level))
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-prompt-tool", "stdio", "--permission-mode", mode, "--include-partial-messages"}
	if o.Ceiling.Clamp(chat.FullAccess) == chat.FullAccess {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	if o.Model != "" && o.Model != "auto" {
		args = append(args, "--model", o.Model)
	}
	if o.ResumeNativeID != "" {
		args = append(args, "--resume", o.ResumeNativeID)
	}
	return args
}
func stripHostSession(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_CHILD_SESSION":
			continue
		}
		out = append(out, entry)
	}
	return out
}

// Probe performs a zero-prompt initialize handshake and joins the probe process.
func (d *Driver) Probe(ctx context.Context) (chat.Probe, error) {
	p := chat.Probe{Capabilities: capabilities()}
	session, err := d.Start(ctx, d.ProbeOptions)
	if err != nil {
		p.Reason = chat.HandshakeFailed
		p.ErrorCode = "initialize_failed"
		return p, err
	}
	s := session.(*Session)
	p.HandshakeOK, p.CLIVersion = true, s.version
	if err := s.Close(); err != nil {
		p.HandshakeOK = false
		return p, err
	}
	return p, nil
}

// Start launches a hardened, owned process and completes its control handshake.
func (d *Driver) Start(ctx context.Context, o chat.StartOptions) (chat.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.Prepare == nil {
		return nil, errors.New("claude: hardened launcher required")
	}
	launch, err := d.Prepare(ctx, o, Args(o))
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		if launch.Cleanup != nil {
			_ = launch.Cleanup()
		}
	}
	if len(launch.Command) == 0 {
		cleanup()
		return nil, errors.New("claude: empty command")
	}
	cmd := exec.Command(launch.Command[0], launch.Command[1:]...)
	cmd.Dir, cmd.Env = launch.Dir, stripHostSession(launch.Env)
	// A nil stderr is an OS null handle, not a copy goroutine descendants can hold open.
	// Explicit pipes allow Wait to reap promptly, even if a descendant retains stdout.
	inR, inW, err := os.Pipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		cleanup()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout = inR, outW
	tree, err := proc.Start(cmd)
	_ = inR.Close()
	_ = outW.Close()
	if err != nil {
		_ = inW.Close()
		_ = outR.Close()
		cleanup()
		return nil, err
	}
	custodyCtx, proof := invoke.WithWorkspaceQuiescence(ctx)
	s := &Session{stream: chat.NewStream(o), options: o, launch: launch, tree: tree, stdin: inW, stdout: outR, pending: map[string]chan controlReply{}, permissions: map[string]object{}, exited: make(chan struct{}), readDone: make(chan struct{}), done: make(chan struct{}), writeQ: make(chan writeRequest, 16), timeout: d.ControlTimeout, proof: proof, acknowledge: invoke.RegisterWorkspaceWriter(custodyCtx), parentAck: invoke.RegisterWorkspaceWriter(ctx)}
	s.norm.resumed = o.ResumeNativeID != ""
	s.norm.nativeID = o.ResumeNativeID
	s.level = o.Ceiling.Clamp(o.Level)
	if s.timeout <= 0 {
		s.timeout = 30 * time.Second
	}
	go func() {
		_ = cmd.Wait()
		s.mu.Lock()
		if !s.closed {
			s.uncontrolledExit = true
			s.failed.Store(true)
		}
		s.mu.Unlock()
		close(s.exited)
		go func() { _ = s.Close() }()
	}()
	go s.writer()
	go s.read()
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
	s.emit("custody.acquired", object{}, "")
	request := object{"subtype": "initialize"}
	if o.SystemPrompt != "" {
		request["appendSystemPrompt"] = o.SystemPrompt
	}
	response, err := s.control(ctx, request)
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	s.version = str(response["claude_code_version"])
	s.emit("session_started", object{"capabilities": capabilities()}, "")
	models := []chat.ModelInfo{}
	for _, raw := range arr(response["models"]) {
		m := obj(raw)
		models = append(models, chat.ModelInfo{ID: str(m["value"]), Label: str(m["displayName"]), Efforts: []string{}})
	}
	s.emit("session_info", object{"models": models, "currentModel": o.Model, "currentEffort": "", "levels": capabilities().LiveLevels, "currentLevel": s.level}, "")
	return s, nil
}

type controlReply struct {
	value object
	err   error
}
type writeRequest struct {
	frame object
	done  chan error
}

// Session owns a long-lived process tree. Native results never release custody.
type Session struct {
	queued                 int
	uncontrolledExit       bool
	failed                 atomic.Bool
	mu                     sync.Mutex
	settingsMu             sync.Mutex
	stream                 *chat.Stream
	options                chat.StartOptions
	launch                 Launch
	tree                   *proc.Tree
	stdin, stdout          *os.File
	norm                   normalizer
	level                  chat.Level
	version                string
	pending                map[string]chan controlReply
	permissions            map[string]object
	counter                uint64
	active, closed         bool
	timeout                time.Duration
	writeQ                 chan writeRequest
	exited, readDone, done chan struct{}
	closeOnce              sync.Once
	closeErr               error
	proof                  *invoke.WorkspaceQuiescence
	acknowledge, parentAck func(error)
}

var _ chat.Driver = (*Driver)(nil)
var _ chat.Session = (*Session)(nil)

// NativeID returns the CLI identity, available after the first system/init frame.
func (s *Session) NativeID() string { s.mu.Lock(); defer s.mu.Unlock(); return s.norm.nativeID }

// Capabilities reports the implemented native operations.
func (s *Session) Capabilities() chat.Capabilities { return capabilities() }

// Events returns the bounded normalized event stream.
func (s *Session) Events() <-chan chat.Event { return s.stream.Events() }
func (s *Session) emit(kind string, data object, command string) {
	if !s.stream.Emit(kind, data, command) {
		if s.failed.CompareAndSwap(false, true) {
			go func() { _ = s.Close() }()
		}
	}
}
func (s *Session) writer() {
	for {
		select {
		case <-s.done:
			return
		case req := <-s.writeQ:
			data, err := json.Marshal(req.frame)
			if err == nil {
				_, err = s.stdin.Write(append(data, '\n'))
				if err != nil {
					s.mu.Lock()
					if !s.closed {
						s.uncontrolledExit = true
						s.failed.Store(true)
					}
					s.mu.Unlock()
				}
			}
			req.done <- err
		}
	}
}
func (s *Session) write(ctx context.Context, frame object) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return chat.ErrClosed
	}
	req := writeRequest{frame: frame, done: make(chan error, 1)}
	select {
	case s.writeQ <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return chat.ErrClosed
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		go func() { _ = s.Close() }()
		return ctx.Err()
	case <-s.done:
		return chat.ErrClosed
	}
}
func (s *Session) control(ctx context.Context, request object) (object, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, chat.ErrClosed
	}
	s.counter++
	id := "goobers-" + strconv.FormatUint(s.counter, 10)
	reply := make(chan controlReply, 1)
	s.pending[id] = reply
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	if err := s.write(ctx, object{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, err
	}
	select {
	case r := <-reply:
		return r.value, r.err
	case <-ctx.Done():
		go func() { _ = s.Close() }()
		return nil, ctx.Err()
	case <-s.done:
		return nil, chat.ErrClosed
	}
}
func (s *Session) read() {
	defer close(s.readDone)
	scan := bufio.NewScanner(s.stdout)
	scan.Buffer(make([]byte, 4096), 4<<20)
	for scan.Scan() {
		var m object
		if json.Unmarshal(scan.Bytes(), &m) != nil || m == nil {
			s.emit("protocol_unknown", unknown("invalid_json").data, "")
			continue
		}
		s.frame(m)
	}
	// EOF and oversized frames both terminate the session and join owned writers.
	s.mu.Lock()
	if !s.closed {
		s.failed.Store(true)
		if scan.Err() == nil {
			s.uncontrolledExit = true
		}
	}
	s.mu.Unlock()
	go func() { _ = s.Close() }()
}
func (s *Session) frame(m object) {
	switch m["type"] {
	case "control_response":
		r := obj(m["response"])
		id := str(r["request_id"])
		s.mu.Lock()
		pending := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if pending == nil {
			s.emit("protocol_unknown", unknown("unmatched_control_response").data, "")
			return
		}
		result := controlReply{value: obj(r["response"])}
		if r["subtype"] != "success" {
			result.err = errors.New("claude: control request rejected")
		}
		pending <- result
	case "control_request":
		s.permissionFrame(m)
	case "control_cancel_request":
		id := str(m["request_id"])
		s.mu.Lock()
		r := s.permissions[id]
		delete(s.permissions, id)
		s.mu.Unlock()
		if r != nil {
			s.resolve(id, r, "cancelled")
		}
	default:
		s.mu.Lock()
		if m["type"] == "system" && m["permissionMode"] != nil {
			actual, known := levelFromMode(str(m["permissionMode"]))
			if !known || s.options.Ceiling.Clamp(actual) != actual {
				s.failed.Store(true)
				s.mu.Unlock()
				s.emit("notice", object{"code": "permission_ceiling_violation"}, "")
				go func() { _ = s.Close() }()
				return
			}
			if s.level != actual {
				s.level = actual
				s.emit("mode_changed", object{"level": actual}, "")
			}
		}
		if !s.active && s.queued > 0 && (m["type"] == "assistant" || m["type"] == "stream_event" || m["type"] == "result") {
			s.active = true
			s.queued--
			s.emit("turn_started", object{}, "")
		}
		events := s.norm.mapFrame(m)
		if m["type"] == "result" {
			s.active = false
			if remaining, ok := m["queued_turn_count"].(float64); ok {
				s.queued = max(0, int(remaining))
			}
		}
		for _, e := range events {
			if e.kind == "turn_ended" {
				e.data["settled"] = s.queued == 0
			}
			s.emit(e.kind, e.data, "")
		}
		if m["type"] == "result" {
			s.emit("custody.pending", object{"reason": "session_process_alive"}, "")
		}
		s.mu.Unlock()
	}
}
func (s *Session) respondAsync(id string, response object) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		response["request_id"] = id
		if err := s.write(ctx, object{"type": "control_response", "response": response}); err != nil {
			_ = s.Close()
		}
	}()
}

// Send delivers text using a prompt or native next/later priority.
func (s *Session) Send(ctx context.Context, blocks []chat.ContentBlock, o chat.SendOptions) (chat.Delivery, error) {
	if _, err := chat.PlainText(blocks); err != nil {
		return "", err
	}
	delivery := o.Delivery
	if delivery == "" {
		delivery = chat.Prompt
	}
	if delivery != chat.Prompt && delivery != chat.Steer && delivery != chat.Queued {
		return "", chat.ErrUnsupported
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", chat.ErrClosed
	}
	if (s.active || s.queued > 0) && delivery == chat.Prompt {
		s.mu.Unlock()
		return "", chat.ErrBusy
	}
	start := !s.active && s.queued == 0
	if start {
		delivery = chat.Prompt
		s.active = true
	} else if delivery == chat.Queued {
		s.queued++
	}
	id := s.norm.nativeID
	s.emit("command.accepted", object{}, o.CommandID)
	if start {
		s.emit("turn_started", object{}, o.CommandID)
	}
	s.emit("user_message", object{"content": blocks, "source": "user", "delivery": delivery}, o.CommandID)
	s.mu.Unlock()
	frame := object{"type": "user", "session_id": id, "parent_tool_use_id": nil, "message": object{"role": "user", "content": blocks}}
	if delivery == chat.Steer {
		frame["priority"] = "next"
	}
	if delivery == chat.Queued {
		frame["priority"] = "later"
	}
	if err := s.write(ctx, frame); err != nil {
		s.emit("command.rejected", object{"code": "send_failed"}, o.CommandID)
		_ = s.Close()
		return "", err
	}
	s.emit("command.delivered", object{"delivery": delivery}, o.CommandID)
	return delivery, nil
}

// Interrupt requests cancellation without claiming writer quiescence.
func (s *Session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	s.norm.interrupting = true
	s.mu.Unlock()
	_, err := s.control(ctx, object{"subtype": "interrupt"})
	return err
}

// SetLevel clamps the requested mode to the immutable host ceiling.
func (s *Session) SetLevel(ctx context.Context, level chat.Level) (chat.Level, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	level = s.options.Ceiling.Clamp(level)
	_, err := s.control(ctx, object{"subtype": "set_permission_mode", "mode": permissionMode(level)})
	if err == nil {
		s.mu.Lock()
		s.level = level
		s.mu.Unlock()
		s.emit("mode_changed", object{"level": level}, "")
	}
	return level, err
}

// SetModel changes the live model; empty or auto selects the CLI default.
func (s *Session) SetModel(ctx context.Context, model string) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	request := object{"subtype": "set_model"}
	if model != "" && model != "auto" {
		request["model"] = model
	}
	_, err := s.control(ctx, request)
	if err == nil {
		s.emit("model_changed", object{"modelId": model}, "")
	}
	return err
}

// SetEffort exposes the native optional setting beyond the common Session seam.
func (s *Session) SetEffort(ctx context.Context, effort string) error {
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return chat.ErrUnsupported
	}
	_, err := s.control(ctx, object{"subtype": "apply_flag_settings", "settings": object{"effortLevel": effort}})
	return err
}
func questions(input object) []object {
	var out []object
	for _, raw := range arr(input["questions"]) {
		q := obj(raw)
		var options []object
		for _, rawOption := range arr(q["options"]) {
			option := obj(rawOption)
			options = append(options, object{"id": str(option["label"]), "label": str(option["label"])})
		}
		out = append(out, object{"questionId": str(q["question"]), "prompt": str(q["question"]), "allowMultiple": q["multiSelect"] == true, "allowFreeText": true, "options": options})
	}
	return out
}
func (s *Session) resolve(id string, r object, decision string) {
	kind := "permission_resolved"
	if r["tool_name"] == "AskUserQuestion" {
		kind = "question_resolved"
	}
	s.emit(kind, object{"requestId": id, "decision": decision}, "")
}
func (s *Session) respond(ctx context.Context, id string, response object, question bool) error {
	s.mu.Lock()
	r := s.permissions[id]
	if r == nil || (r["tool_name"] == "AskUserQuestion") != question {
		s.mu.Unlock()
		return chat.ErrUnknownRequest
	}
	delete(s.permissions, id)
	s.mu.Unlock()
	if response["behavior"] == "allow" && response["updatedInput"] == nil {
		response["updatedInput"] = obj(r["input"])
	}
	err := s.write(ctx, object{"type": "control_response", "response": object{"subtype": "success", "request_id": id, "response": response}})
	if err != nil {
		_ = s.Close()
		return err
	}
	s.resolve(id, r, str(response["behavior"]))
	return nil
}

// RespondPermission answers a pending permission without persistent grants.
func (s *Session) RespondPermission(ctx context.Context, id string, r chat.PermissionResponse) error {
	switch r.OptionID {
	case "allow_once", "allow":
		response := object{"behavior": "allow"}
		if r.UpdatedInput != nil {
			response["updatedInput"] = r.UpdatedInput
		}
		return s.respond(ctx, id, response, false)
	case "deny", "deny_once", "reject_once":
		message := r.Message
		if message == "" {
			message = "Denied by user"
		}
		return s.respond(ctx, id, object{"behavior": "deny", "message": message}, false)
	default:
		return chat.ErrUnsupported
	}
}

// AnswerQuestion echoes the original question input with the selected answers.
func (s *Session) AnswerQuestion(ctx context.Context, id string, answers chat.QuestionAnswers) error {
	if answers == nil {
		return s.respond(ctx, id, object{"behavior": "deny", "message": "Question cancelled by host policy"}, true)
	}
	s.mu.Lock()
	r := s.permissions[id]
	input := obj(r["input"])
	s.mu.Unlock()
	updated := object{}
	for k, v := range input {
		updated[k] = v
	}
	wireAnswers := object{}
	for k, v := range answers {
		wireAnswers[k] = strings.Join(v, ", ")
	}
	updated["answers"] = wireAnswers
	return s.respond(ctx, id, object{"behavior": "allow", "updatedInput": updated}, true)
}

// Close stops and joins the owned process tree before releasing workspace custody.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		// Capture and stop descendants BEFORE closing stdin can orphan them.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.tree.StopAndWait(ctx)
		cancel()
		if err != nil {
			err = errors.Join(err, s.tree.Kill())
		}
		s.mu.Lock()
		uncontrolled := s.uncontrolledExit
		s.mu.Unlock()
		if uncontrolled {
			err = errors.Join(err, errors.New("claude: root exited before descendant custody was captured"))
		}
		_ = s.stdin.Close()
		_ = s.stdout.Close()
		select {
		case <-s.exited:
		case <-time.After(5 * time.Second):
			err = errors.Join(err, errors.New("claude: process wait timed out"))
		}
		<-s.readDone
		s.acknowledge(err)
		if s.parentAck != nil {
			s.parentAck(err)
		}
		s.closeErr = s.proof.Verify()
		s.mu.Lock()
		active := s.active
		s.active = false
		permissions := s.permissions
		s.permissions = map[string]object{}
		s.mu.Unlock()
		for id, r := range permissions {
			s.resolve(id, r, "cancelled")
		}
		if active {
			reason := "cancelled"
			if s.failed.Load() {
				reason = "error"
				s.emit("error", object{"code": "driver_stream_failed", "message": "Claude stream ended before a result", "fatal": true}, "")
			}
			s.emit("turn_ended", object{"reason": reason}, "")
		}
		if s.closeErr == nil {
			s.emit("custody.released", object{}, "")
			if s.launch.Cleanup != nil {
				s.closeErr = s.launch.Cleanup()
			}
		} else {
			s.emit("custody.pending", object{"reason": "unverified_process_tree"}, "")
		}
		s.emit("session_ended", object{}, "")
		s.stream.Close()
		close(s.done)
	})
	return s.closeErr
}

func (s *Session) permissionFrame(m object) {
	r := obj(m["request"])
	id := str(m["request_id"])
	if r["subtype"] != "can_use_tool" || id == "" {
		s.emit("protocol_unknown", unknown("control_request/"+str(r["subtype"])).data, "")
		s.respondAsync(id, object{"subtype": "error", "error": "unsupported control request"})
		return
	}
	s.mu.Lock()
	_, duplicate := s.permissions[id]
	if !duplicate {
		s.permissions[id] = r
	}
	s.mu.Unlock()
	if duplicate {
		s.emit("protocol_unknown", unknown("duplicate_control_request").data, "")
		go func() { _ = s.Close() }()
		return
	}
	data := object{"requestId": id, "toolCallId": str(r["tool_use_id"]), "title": str(r["tool_name"]), "kind": "tool", "content": permissionPreview(str(r["tool_name"]), obj(r["input"])), "options": []object{{"optionId": "allow_once", "kind": "allow_once", "label": "Allow once"}, {"optionId": "reject_once", "kind": "reject_once", "label": "Deny"}}}
	kind := "permission_request"
	if r["tool_name"] == "AskUserQuestion" {
		kind = "question"
		data["questions"] = questions(obj(r["input"]))
	}
	s.emit(kind, data, "")
}
