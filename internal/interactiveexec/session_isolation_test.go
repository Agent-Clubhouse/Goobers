//go:build !windows

package interactiveexec

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/interactiveagent"
	"github.com/goobers/goobers/internal/sandbox"
)

func TestNativeTwoSessionReadAndBridgeIsolation(t *testing.T) {
	base, err := sandbox.New()
	if errors.Is(err, sandbox.ErrUnavailable) || errors.Is(err, sandbox.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	e, _ := bridgeExecutor(t)
	e.config.Sandbox = base
	root, err := e.bridgeRuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	var checkedA, invokedA, checkedB, invokedB atomic.Int32
	start := func(other bool) *interactiveagent.Bridge {
		b, err := interactiveagent.Start(t.Context(), interactiveagent.Options{RuntimeDir: root,
			Tools: []interactiveagent.Tool{{Name: "ping", Description: "Fixture", InputSchema: map[string]any{"type": "object", "additionalProperties": false}}},
			Check: func(context.Context) error {
				if other {
					checkedB.Add(1)
				} else {
					checkedA.Add(1)
				}
				return nil
			},
			Invoke: func(context.Context, interactiveagent.Call) (any, error) {
				if other {
					invokedB.Add(1)
				} else {
					invokedA.Add(1)
				}
				return map[string]any{"ok": true}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}
	a, b := start(false), start(true)
	workspace, _ := e.workspace("session-a")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	boundary, err := e.nativeSandbox()
	if err != nil {
		t.Fatal(err)
	}
	boundary, err = e.sessionSandbox(boundary, workspace, filepath.Dir(a.TokenPath))
	if err != nil {
		t.Fatal(err)
	}
	otherWorkspace, _ := e.workspace("session-b")
	otherState := chatStatePath(e.config.StateDir, otherWorkspace)
	secretPaths := []string{filepath.Join(otherWorkspace, "private"), filepath.Join(otherState, "private"), filepath.Join(e.config.StateDir, "human-principal-secret")}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSessionIsolationChild$")
	command.Dir = workspace
	command.Env = append(os.Environ(), "GOOBERS_ISOLATION_CHILD=1", "ISOLATION_A_TOKEN="+a.TokenPath,
		"ISOLATION_A_SOCKET="+a.SocketPath, "ISOLATION_B_TOKEN="+b.TokenPath, "ISOLATION_B_SOCKET="+b.SocketPath,
		"ISOLATION_B_WORKSPACE="+secretPaths[0], "ISOLATION_B_STATE="+secretPaths[1], "ISOLATION_HUMAN="+secretPaths[2])
	if err := boundary.Wrap(command, sandbox.Policy{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	// Create the sibling AFTER policy compilation. Enumeration-based denials
	// would miss it, whereas root exceptions still protect it.
	for _, path := range secretPaths {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture-private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("two-session kernel proof: %v\n%s", err, output)
	}
	if checkedB.Load() != 0 || invokedB.Load() != 0 {
		t.Fatalf("foreign bridge reached policy/invoke: %d/%d", checkedB.Load(), invokedB.Load())
	}
	if checkedA.Load() == 0 || invokedA.Load() == 0 {
		t.Fatalf("own bridge did not reach policy/invoke: %d/%d", checkedA.Load(), invokedA.Load())
	}
}

func TestSessionIsolationChild(t *testing.T) {
	if os.Getenv("GOOBERS_ISOLATION_CHILD") != "1" {
		t.Skip("subprocess fixture")
	}
	for _, key := range []string{"ISOLATION_B_WORKSPACE", "ISOLATION_B_STATE", "ISOLATION_B_TOKEN", "ISOLATION_HUMAN"} {
		if _, err := os.ReadFile(os.Getenv(key)); err == nil {
			t.Fatalf("read foreign authority via %s", key)
		}
	}
	token, err := os.ReadFile(os.Getenv("ISOLATION_A_TOKEN"))
	if err != nil {
		t.Fatal("own bridge token unavailable", err)
	}
	if err := isolationBridgeCall(os.Getenv("ISOLATION_A_SOCKET"), token); err != nil {
		t.Fatal("own bridge unavailable", err)
	}
	if err := isolationBridgeCall(os.Getenv("ISOLATION_B_SOCKET"), token); err == nil {
		t.Fatal("foreign bridge accepted own invocation authority")
	}
	if err := os.WriteFile("own-output", []byte("allowed"), 0600); err != nil {
		t.Fatal("own workspace unavailable", err)
	}
}

func isolationBridgeCall(socket string, token []byte) error {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write(token); err != nil {
		return err
	}
	var ack [3]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		return err
	}
	if string(ack[:]) != "OK\n" {
		return errors.New("authentication rejected")
	}
	reader := bufio.NewReader(conn)
	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping","arguments":{}}}`,
	}
	for _, request := range requests {
		if _, err := io.WriteString(conn, request+"\n"); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return err
		}
		var response struct {
			Error  json.RawMessage `json:"error"`
			Result map[string]any  `json:"result"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			return err
		}
		if len(response.Error) != 0 || response.Result == nil || response.Result["isError"] == true {
			return errors.New("bridge did not return a successful MCP result")
		}
	}
	return nil
}
