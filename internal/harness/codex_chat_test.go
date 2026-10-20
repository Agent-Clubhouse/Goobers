package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/chat/chattest"
)

// A real child process exercises JSONL framing, request correlation, process
// ownership and native resume without network access or a model credential.
func TestCodexChatHelper(t *testing.T) {
	if os.Getenv("GOOBERS_CHAT_FIXTURE") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	initialized := false
	encode := json.NewEncoder(os.Stdout)
	send := func(v any) {
		if encode.Encode(v) != nil {
			os.Exit(2)
		}
	}
	turn := 0
	for scanner.Scan() {
		var raw map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &raw) != nil || raw["jsonrpc"] != nil {
			os.Exit(3)
		}
		var m codexRPCMessage
		_ = json.Unmarshal(scanner.Bytes(), &m)
		if m.Method == "initialized" {
			initialized = true
			continue
		}
		if m.Method == "initialize" {
			send(map[string]any{"id": m.ID, "result": map[string]any{"userAgent": "codex-cli 0.163.0"}})
			continue
		}
		if !initialized {
			os.Exit(4)
		}
		var result any
		switch m.Method {
		case "account/read":
			result = map[string]any{"account": nil}
		case "thread/start", "thread/resume":
			var p map[string]any
			_ = json.Unmarshal(m.Params, &p)
			if p["approvalPolicy"] != "never" || p["developerInstructions"] == nil {
				os.Exit(5)
			}
			if m.Method == "thread/resume" && p["excludeTurns"] != true {
				os.Exit(6)
			}
			result = map[string]any{"thread": map[string]any{"id": "thread-fixture"}, "model": "fixture-model"}
		case "model/list":
			result = map[string]any{"data": []any{map[string]any{"id": "fixture-model", "model": "fixture-model", "defaultReasoningEffort": "medium", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "medium"}, map[string]any{"reasoningEffort": "high"}}}}}
		case "turn/start":
			var p map[string]any
			_ = json.Unmarshal(m.Params, &p)
			sandbox, _ := p["sandboxPolicy"].(map[string]any)
			if p["approvalPolicy"] != "never" || sandbox["networkAccess"] != false || p["effort"] == nil {
				os.Exit(7)
			}
			turn++
			id := fmt.Sprintf("turn-%d", turn)
			send(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "thread-fixture", "turn": map[string]any{"id": id, "status": "inProgress"}}})
			result = map[string]any{"turn": map[string]any{"id": id, "status": "inProgress"}}
		case "turn/steer":
			var p map[string]any
			_ = json.Unmarshal(m.Params, &p)
			id := fmt.Sprintf("turn-%d", turn)
			if os.Getenv("GOOBERS_CHAT_REJECT_STEER") == "1" {
				send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "steer rejected"}})
				continue
			}
			if p["expectedTurnId"] != id {
				os.Exit(8)
			}
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-fixture", "turn": map[string]any{"id": id, "status": "completed"}}})
			send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "stale turn"}})
			continue
		case "turn/interrupt":
			result = map[string]any{}
		default:
			result = map[string]any{}
		}
		send(map[string]any{"id": m.ID, "result": result})
	}
	os.Exit(0)
}

func codexChatFixture(t *testing.T) CodexChatConfig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("process-tree quiescence proof is not supported on Windows")
	}
	t.Setenv("GOOBERS_CHAT_FIXTURE", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return CodexChatConfig{Adapter: &CodexAdapter{Command: []string{exe, "-test.run=^TestCodexChatHelper$", "--"}, ExtraEnvAllowlist: []string{"GOOBERS_CHAT_FIXTURE"}, ModelCredential: func(context.Context) (string, error) { return "sk-fixture-only", nil }}, Request: RunRequest{Workspace: t.TempDir(), Model: "fixture-model"}}
}
func TestCodexChatConformance(t *testing.T) {
	cfg := codexChatFixture(t)
	chattest.Conformance(t, func() chat.Driver { return NewCodexChatDriver(cfg) })
}
func TestCodexChatTurnsAndResume(t *testing.T) {
	cfg := codexChatFixture(t)
	cfg.StateDir = t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	driver := NewCodexChatDriver(cfg)
	opts := chat.StartOptions{Level: chat.Auto, Ceiling: chat.NewCeiling(chat.Auto, chat.Auto, chat.Auto)}
	session, err := driver.Start(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	s := session.(*codexChatSession)
	if err := s.SetEffort(ctx, "high"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEffort(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if s.effort != "medium" {
		t.Fatal("empty effort did not restore concrete default")
	}
	if _, err = s.Send(ctx, chat.Text("first"), chat.SendOptions{Delivery: chat.Prompt, CommandID: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Send(ctx, chat.Text("busy"), chat.SendOptions{Delivery: chat.Prompt}); !errors.Is(err, chat.ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	delivery, err := s.Send(ctx, chat.Text("next"), chat.SendOptions{Delivery: chat.Steer, CommandID: "next"})
	if err != nil || delivery != chat.Prompt {
		t.Fatalf("stale steer fallback: %s %v", delivery, err)
	}
	if err = s.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	opts.ResumeNativeID = s.NativeID()
	resumed, err := driver.Start(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = resumed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(cfg.StateDir, "home", "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private auth file exists: %v", err)
	}
}
func TestCodexChatPrivateAuthAndState(t *testing.T) {
	cfg := codexChatFixture(t)
	cfg.StateDir = t.TempDir()
	ambient := t.TempDir()
	auth := []byte(`{"refresh_token":"fixture-must-not-copy"}`)
	if err := os.WriteFile(filepath.Join(ambient, "auth.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", ambient)
	cfg.Request.HarnessOptions = testHarnessOptions(t, map[string]interface{}{"auth": "ambient-chatgpt", "allowFileBackedCredentials": true})
	p, err := prepareCodexChat(context.Background(), cfg, chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.argv, " "), `cli_auth_credentials_store="keyring"`) {
		t.Fatal("missing keyring isolation")
	}
	if _, err := os.Stat(filepath.Join(p.home, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("auth copied")
	}
	if _, _, err := codexChatState(cfg.StateDir, ""); err == nil {
		t.Fatal("concurrent state owner allowed")
	}
	p.cleanup()
	_, err = NewCodexChatDriver(cfg).Start(context.Background(), chat.StartOptions{})
	if err == nil || !strings.Contains(err.Error(), "OS keyring") {
		t.Fatalf("missing login diagnostic: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ambient, "auth.json"))
	if err != nil || string(got) != string(auth) {
		t.Fatal("ambient login modified")
	}
}

// Opt-in protocol smoke: no turn or model request is sent. The key is a fake
// fixture value, so this cannot spend credentials or refresh a real login.
func TestCodexChatInstalledSmoke(t *testing.T) {
	if os.Getenv("GOOBERS_CODEX_CHAT_SMOKE") != "1" {
		t.Skip("opt-in installed CLI smoke")
	}
	cfg := CodexChatConfig{Adapter: &CodexAdapter{Command: []string{"codex"}, ModelCredential: func(context.Context) (string, error) { return "sk-fixture-no-model-call", nil }}, Request: RunRequest{Workspace: t.TempDir()}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := NewCodexChatDriver(cfg).Start(ctx, chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.NativeID() == "" {
		t.Fatal("missing native thread")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexChatRejectedSteerDoesNotStartAnotherTurn(t *testing.T) {
	cfg := codexChatFixture(t)
	t.Setenv("GOOBERS_CHAT_REJECT_STEER", "1")
	cfg.Adapter.ExtraEnvAllowlist = append(cfg.Adapter.ExtraEnvAllowlist, "GOOBERS_CHAT_REJECT_STEER")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := NewCodexChatDriver(cfg).Start(ctx, chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if _, err = session.Send(ctx, chat.Text("first"), chat.SendOptions{Delivery: chat.Prompt}); err != nil {
		t.Fatal(err)
	}
	if _, err = session.Send(ctx, chat.Text("steer"), chat.SendOptions{Delivery: chat.Steer}); err == nil {
		t.Fatal("rejected steer delivered")
	}
	s := session.(*codexChatSession)
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != "turn-1" {
		t.Fatalf("unexpected turn after steer rejection: %s", active)
	}
}
func TestCodexChatOverflowStopsWriterWithoutSubscriber(t *testing.T) {
	cfg := codexChatFixture(t)
	session, err := NewCodexChatDriver(cfg).Start(context.Background(), chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := session.(*codexChatSession)
	for range 1025 {
		s.emit("notice", map[string]any{"message": "overflow"}, "")
	}
	select {
	case <-s.waitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("writer survived event overflow")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestCodexChatStdioConfigUsesArraysAndCredentialNames(t *testing.T) {
	cfg := codexChatStdioServer("tool", nil, nil, nil)
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Fatalf("null lists are not valid Codex config: %s", b)
	}
	cfg = codexChatStdioServer("tool", []string{"arg"}, []string{"MCP_TOKEN"}, nil)
	if cfg["required"] != true || cfg["env_vars"].([]string)[0] != "MCP_TOKEN" {
		t.Fatal(cfg)
	}
}
