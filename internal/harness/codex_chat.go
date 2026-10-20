package harness

// The native lifecycle, stale-turn handling and sticky overrides adapt
// Clubhouse's codex-chat-driver.ts (MIT), commit 3309665c. Process custody,
// credential isolation and immutable ceilings are Goobers-owned boundaries.
// See internal/codexprotocol/CLUBHOUSE-LICENSE.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/chat"
	cp "github.com/goobers/goobers/internal/codexprotocol"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
)

// CodexChatDriver implements the native app-server transport under "codex".
type CodexChatDriver struct {
	config CodexChatConfig
	start  func(context.Context, CodexChatConfig, chat.StartOptions) (*codexChatSession, error)
}

// NewCodexChatDriver binds a native app-server driver to host invocation authority.
func NewCodexChatDriver(config CodexChatConfig) *CodexChatDriver {
	return &CodexChatDriver{config: config, start: startCodexChat}
}

// ID identifies this native transport independently of the provider key.
func (*CodexChatDriver) ID() string { return "codex_app_server" }

func codexChatCapabilities(persistent bool) chat.Capabilities {
	c := chat.MinimalCapabilities()
	c.Steer, c.Interrupt, c.Questions, c.SystemPrompt = true, true, true, true
	c.Queue, c.Usage, c.ModelSwitch, c.EffortSwitch = "none", "tokens", "per_turn", "per_turn"
	c.LiveLevels = []chat.Level{chat.Plan, chat.Auto}
	if persistent {
		c.Resume = "native"
	}
	return c
}

// Probe verifies the installed transport in disposable state beneath the host-owned session root.
func (d *CodexChatDriver) Probe(ctx context.Context) (chat.Probe, error) {
	probe := chat.Probe{Capabilities: codexChatCapabilities(d.config.StateDir != "")}
	config := d.config
	config.StateDir = "" // Disposable child state never locks or modifies the retained home.
	if d.config.StateDir != "" {
		if !filepath.IsAbs(d.config.StateDir) {
			probe.Reason = chat.StartFailed
			return probe, errors.New("codex chat StateDir must be absolute")
		}
		if err := privateCodexDirectory(d.config.StateDir); err != nil {
			probe.Reason = chat.StartFailed
			return probe, err
		}
		if err := validateCodexChatState(d.config.StateDir, config.Request.Workspace); err != nil {
			probe.Reason = chat.StartFailed
			return probe, err
		}
		if config.Adapter != nil {
			adapter := *config.Adapter
			// The batch adapter may put ephemeral files under workspace/.tmp.
			// Keep probes inside the session's outer-sandbox exception.
			adapter.EphemeralTmpRoot = d.config.StateDir
			config.Adapter = &adapter
		}
	}
	session, err := d.start(ctx, config, chat.StartOptions{Workspace: config.Request.Workspace})
	if err != nil {
		probe.Reason = chat.HandshakeFailed
		var missing *exec.Error
		if errors.As(err, &missing) {
			probe.Reason = chat.BinaryMissing
		}
		return probe, err
	}
	probe.CLIVersion = session.cliVersion
	if err := session.Close(); err != nil {
		probe.Reason = chat.StartFailed
		return probe, err
	}
	probe.HandshakeOK = true
	return probe, nil
}

// Start acquires process custody and creates or resumes one native thread.
func (d *CodexChatDriver) Start(ctx context.Context, options chat.StartOptions) (chat.Session, error) {
	s, err := d.start(ctx, d.config, options)
	if err != nil {
		return nil, err
	}
	if err := s.openThread(ctx, options); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

type codexChatSession struct {
	rpc         *codexRPC
	tree        *proc.Tree
	waitDone    chan struct{}
	proof       *invoke.WorkspaceQuiescence
	acknowledge func(error)
	prepared    preparedCodexChat
	caps        chat.Capabilities
	stream      *chat.Stream
	cliVersion  string
	closeOnce   sync.Once
	closeErr    error
	closing     atomic.Bool
	sendMu      sync.Mutex
	// requestMu orders question replies against lifecycle invalidation. Never
	// hold it while waiting for a native RPC response (the reader drains turns).
	requestMu     sync.Mutex
	mu            sync.Mutex
	blockedTurn   string
	questionSeq   uint64
	nativeID      string
	active        string
	lastEnded     string
	level         chat.Level
	ceiling       chat.Ceiling
	model         string
	effort        string
	defaultEffort bool
	catalog       []cp.Model
	early         []codexRPCMessage
	questions     map[string]codexChatQuestion
	erroredTurn   string
	usage         cp.TokenUsageBreakdown
	usageKnown    bool
}

func startCodexChat(ctx context.Context, config CodexChatConfig, o chat.StartOptions) (*codexChatSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := prepareCodexChat(ctx, config, o)
	if err != nil {
		return nil, err
	}
	s := &codexChatSession{
		prepared: p, caps: codexChatCapabilities(config.StateDir != ""), stream: chat.NewStream(o),
		level: o.Ceiling.Clamp(o.Level), ceiling: o.Ceiling, model: p.model, effort: p.effort,
		defaultEffort: p.effort == "", waitDone: make(chan struct{}),
		questions: map[string]codexChatQuestion{},
	}
	// Native and outer sandboxes remain workspace-limited even if a caller
	// presents a full-access ceiling. This driver never grants network access.
	switch s.level {
	case chat.FullAccess:
		s.level = chat.Auto
	case chat.Ask, chat.AutoEdit:
		s.level = chat.Plan
	}
	cmd := exec.Command(p.argv[0], p.argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stderr, cmd.WaitDelay = p.workspace, p.env, io.Discard, groupKillWaitDelay
	input, err := cmd.StdinPipe()
	if err != nil {
		p.cleanup()
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		p.cleanup()
		return nil, err
	}
	s.tree, err = proc.Start(cmd)
	if err != nil {
		_ = input.Close()
		_ = output.Close()
		p.cleanup()
		return nil, err
	}
	hostAck := invoke.RegisterWorkspaceWriter(ctx)
	localCtx, proof := invoke.WithWorkspaceQuiescence(ctx)
	localAck := invoke.RegisterWorkspaceWriter(localCtx)
	s.proof = proof
	s.acknowledge = func(err error) {
		localAck(err)
		if hostAck != nil {
			hostAck(err)
		}
	}
	s.rpc = newCodexRPC(input, s.receive, s.transportFailed)
	s.rpc.start(output)
	go func() {
		<-s.rpc.readDone
		_ = cmd.Wait()
		close(s.waitDone)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.waitDone:
		}
	}()
	var initialized cp.InitializeResponse
	init := cp.InitializeParams{ClientInfo: cp.ClientInfo{Name: "goobers", Version: "1"}}
	if err := s.rpc.request(ctx, "initialize", init, &initialized); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	s.cliVersion = initialized.UserAgent
	if err := s.rpc.send(ctx, codexRPCMessage{Method: "initialized"}); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if p.ambient {
		var account struct {
			Account any `json:"account"`
		}
		if err := s.rpc.request(ctx, "account/read", map[string]any{"refreshToken": false}, &account); err != nil || account.Account == nil {
			return nil, errors.Join(errors.New("codex chat requires a ChatGPT login available from the OS keyring in its private home; file-backed login is not copied"), err, s.Close())
		}
	}
	return s, nil
}

func (s *codexChatSession) openThread(ctx context.Context, o chat.StartOptions) error {
	policy, mode, _ := codexChatPolicy(s.level, s.prepared.workspace)
	config := map[string]json.RawMessage{"mcp_servers": codexRaw(s.prepared.mcp)}
	var response cp.ThreadStartResponse
	if o.ResumeNativeID != "" {
		if s.caps.Resume != "native" {
			return fmt.Errorf("codex chat native resume requires host-owned StateDir: %w", chat.ErrUnsupported)
		}
		request := cp.ThreadResumeParams{
			ThreadId: o.ResumeNativeID, ExcludeTurns: true, Cwd: &s.prepared.workspace, Model: optionalCodexString(s.model),
			DeveloperInstructions: &s.prepared.prompt, ApprovalPolicy: &policy, Sandbox: &mode, Config: &config,
		}
		if err := s.rpc.request(ctx, "thread/resume", request, &response); err != nil {
			return err
		}
	} else {
		request := cp.ThreadStartParams{
			Cwd: &s.prepared.workspace, Model: optionalCodexString(s.model), DeveloperInstructions: &s.prepared.prompt,
			ApprovalPolicy: &policy, Sandbox: &mode, Config: &config,
		}
		if err := s.rpc.request(ctx, "thread/start", request, &response); err != nil {
			return err
		}
	}
	if response.Thread.Id == "" {
		return errors.New("codex app-server returned no thread ID")
	}
	s.mu.Lock()
	s.nativeID, s.model = response.Thread.Id, response.Model
	early := s.early
	s.early = nil
	s.mu.Unlock()
	s.emit("session_started", map[string]any{"driver": "codex_app_server", "nativeSessionId": response.Thread.Id, "resumed": o.ResumeNativeID != "", "capabilities": s.caps}, "")
	for _, msg := range early {
		s.receive(msg)
	}
	var catalog []cp.Model
	request := cp.ModelListParams{}
	cursors := map[string]bool{}
	for {
		var models cp.ModelListResponse
		if err := s.rpc.request(ctx, "model/list", request, &models); err != nil {
			return err
		}
		catalog = append(catalog, models.Data...)
		if len(catalog) > 1024 {
			return errors.New("codex model catalog exceeds limit")
		}
		if models.NextCursor == nil || *models.NextCursor == "" {
			break
		}
		if cursors[*models.NextCursor] {
			return errors.New("codex model catalog repeats a cursor")
		}
		cursors[*models.NextCursor] = true
		request.Cursor = models.NextCursor
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catalog = catalog
	row := s.modelRow(s.model)
	if row == nil {
		return errors.New("codex active model is absent from its catalog")
	}
	if s.defaultEffort {
		s.effort = string(row.DefaultReasoningEffort)
	}
	if s.effort == "" || !codexSupportsEffort(row, s.effort) {
		return errors.New("codex active model has no supported reasoning effort")
	}

	models := make([]chat.ModelInfo, 0, len(catalog))
	for _, m := range catalog {
		efforts := make([]string, 0, len(m.SupportedReasoningEfforts))
		for _, effort := range m.SupportedReasoningEfforts {
			efforts = append(efforts, string(effort.ReasoningEffort))
		}
		models = append(models, chat.ModelInfo{ID: m.Model, Label: m.DisplayName, Efforts: efforts, DefaultEffort: string(m.DefaultReasoningEffort)})
	}
	s.emit("session_info", map[string]any{"models": models, "currentModel": s.model, "currentEffort": s.effort, "levels": s.caps.LiveLevels, "currentLevel": s.level}, "")
	return nil
}

func optionalCodexString(value string) *string {
	if value == "" || value == "auto" {
		return nil
	}
	return &value
}

func (s *codexChatSession) NativeID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nativeID
}
func (s *codexChatSession) Capabilities() chat.Capabilities { return s.caps }
func (s *codexChatSession) Events() <-chan chat.Event       { return s.stream.Events() }

func (s *codexChatSession) Send(ctx context.Context, content []chat.ContentBlock, o chat.SendOptions) (chat.Delivery, error) {
	text, err := chat.PlainText(content)
	if err != nil {
		return "", err
	}
	if o.Delivery != chat.Prompt && o.Delivery != chat.Steer {
		return "", chat.ErrUnsupported
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.closing.Load() {
		return "", chat.ErrClosed
	}
	s.mu.Lock()
	id, active, model, effort, level := s.nativeID, s.active, s.model, s.effort, s.level
	s.mu.Unlock()
	input := []cp.UserInput{codexRaw(map[string]any{"type": "text", "text": text, "text_elements": []any{}})}
	delivery := chat.Prompt
	if active != "" {
		if o.Delivery != chat.Steer {
			return "", chat.ErrBusy
		}
		var result cp.TurnSteerResponse
		err := s.rpc.request(ctx, "turn/steer", cp.TurnSteerParams{ThreadId: id, ExpectedTurnId: active, Input: input}, &result)
		if err == nil {
			delivery = chat.Steer
		} else {
			s.mu.Lock()
			stale := s.active == "" && s.lastEnded == active
			s.mu.Unlock()
			var rpcErr *codexRPCError
			if !stale || !errors.As(err, &rpcErr) {
				return "", err
			}
		}
	}
	if delivery == chat.Prompt {
		approval, _, sandbox := codexChatPolicy(level, s.prepared.workspace)
		request := cp.TurnStartParams{ThreadId: id, Input: input, Model: optionalCodexString(model), ApprovalPolicy: &approval, SandboxPolicy: &sandbox}
		if effort != "" {
			e := cp.ReasoningEffort(effort)
			request.Effort = &e
		}
		var response cp.TurnStartResponse
		if err := s.rpc.request(ctx, "turn/start", request, &response); err != nil {
			return "", err
		}
		if response.Turn.Status == "inProgress" {
			s.activateTurn(response.Turn.Id)
		}
	}
	s.emit("user_message", map[string]any{"content": content, "source": "user", "delivery": delivery}, o.CommandID)
	return delivery, nil
}

func (s *codexChatSession) Interrupt(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closing.Load() {
		return chat.ErrClosed
	}
	s.mu.Lock()
	id, turn := s.nativeID, s.active
	s.mu.Unlock()
	if turn == "" {
		return nil
	}
	s.cancelQuestions(turn) // Invalidate at admission, before waiting for native acknowledgement.
	return s.rpc.request(ctx, "turn/interrupt", cp.TurnInterruptParams{ThreadId: id, TurnId: turn}, nil)
}

func (s *codexChatSession) Close() error {
	s.closeOnce.Do(func() {
		s.requestMu.Lock()
		s.closing.Store(true)
		s.requestMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := s.tree.StopAndWait(ctx)
		if err != nil {
			_ = s.tree.Kill()
		}
		s.rpc.fail(chat.ErrClosed)
		_ = s.rpc.input.Close()
		select {
		case <-s.waitDone:
		case <-ctx.Done():
			err = errors.Join(err, ctx.Err())
		}
		s.acknowledge(err)
		s.closeErr = s.proof.Verify()
		if s.closeErr == nil {
			s.prepared.cleanup()
		}
		s.cancelQuestions("")
		s.mu.Lock()
		active := s.active
		s.active = ""
		s.mu.Unlock()
		if active != "" {
			s.emit("turn_ended", map[string]any{"nativeTurnId": active, "reason": "cancelled"}, "")
		}
		s.emit("session_ended", map[string]any{"reason": "stopped"}, "")
		s.stream.Close()
	})
	return s.closeErr
}

func (s *codexChatSession) transportFailed(_ error) {
	if !s.closing.Load() {
		s.emit("error", map[string]any{"code": "process_exited", "message": "Codex app-server transport ended", "fatal": true}, "")
	}
	_ = s.Close()
}

func (s *codexChatSession) emit(kind string, data map[string]any, command string) {
	if !s.stream.Emit(kind, data, command) && !s.closing.Load() {
		go func() { _ = s.Close() }()
	}
}

func codexChatPolicy(level chat.Level, workspace string) (cp.AskForApproval, cp.SandboxMode, cp.SandboxPolicy) {
	approval, mode := "never", cp.SandboxMode("workspace-write")
	policy := map[string]any{"type": "workspaceWrite", "writableRoots": []string{workspace}, "networkAccess": false, "excludeTmpdirEnvVar": true, "excludeSlashTmp": true}
	switch level {
	case chat.Plan:
		mode = "read-only"
		policy = map[string]any{"type": "readOnly", "networkAccess": false}
	}
	return codexRaw(approval), mode, codexRaw(policy)
}

func (s *codexChatSession) SetLevel(_ context.Context, level chat.Level) (chat.Level, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.closing.Load() {
		return "", chat.ErrClosed
	}
	level = s.ceiling.Clamp(level)
	if level == chat.Ask || level == chat.AutoEdit {
		return "", chat.ErrUnsupported
	}
	if level == chat.FullAccess {
		level = chat.Auto
	}
	s.mu.Lock()
	s.level = level
	s.mu.Unlock()
	s.emit("mode_changed", map[string]any{"level": level}, "")
	return level, nil
}

func (s *codexChatSession) modelRow(model string) *cp.Model {
	for i := range s.catalog {
		if s.catalog[i].Model == model || s.catalog[i].Id == model {
			return &s.catalog[i]
		}
	}
	return nil
}

func (s *codexChatSession) SetModel(_ context.Context, model string) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Load() {
		return chat.ErrClosed
	}
	row := s.modelRow(model)
	if row == nil {
		return fmt.Errorf("codex chat model is not in the server catalog: %w", chat.ErrUnsupported)
	}
	if s.defaultEffort || !codexSupportsEffort(row, s.effort) {
		s.effort, s.defaultEffort = string(row.DefaultReasoningEffort), true
	}
	s.model = row.Model
	s.emit("model_changed", map[string]any{"modelId": s.model, "effort": s.effort}, "")
	return nil
}

// SetEffort implements the optional chat.EffortSetter contract. Empty means
// the catalog's concrete default; null would leave the prior override sticky.
func (s *codexChatSession) SetEffort(_ context.Context, effort string) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Load() {
		return chat.ErrClosed
	}
	row := s.modelRow(s.model)
	if row == nil {
		return chat.ErrUnsupported
	}
	useDefault := effort == ""
	if useDefault {
		effort = string(row.DefaultReasoningEffort)
	}
	if effort == "" || !codexSupportsEffort(row, effort) {
		return chat.ErrUnsupported
	}
	s.effort, s.defaultEffort = effort, useDefault
	s.emit("model_changed", map[string]any{"modelId": s.model, "effort": effort, "effortIsDefault": useDefault}, "")
	return nil
}

func codexSupportsEffort(row *cp.Model, effort string) bool {
	for _, option := range row.SupportedReasoningEfforts {
		if string(option.ReasoningEffort) == effort {
			return true
		}
	}
	return false
}
