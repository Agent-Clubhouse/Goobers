//go:build !windows

package interactiveagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Knowing another invocation's socket and possessing a valid token for one's
// own invocation must never grant the other invocation's host authority.
func TestTwoBridgesRejectForeignAndMissingCredentials(t *testing.T) {
	var checks, calls atomic.Int32
	attacker := startTest(t, Options{})
	victim := startTest(t, Options{
		Check: func(context.Context) error { checks.Add(1); return nil },
		Invoke: func(context.Context, Call) (any, error) {
			calls.Add(1)
			return "victim-private-fixture", nil
		},
	})
	foreign, err := readToken(attacker.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n"
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"probe"}}}` + "\n"
	for name, prefix := range map[string]string{
		"missing":   "",
		"foreign":   string(foreign),
		"malformed": strings.Repeat("!", tokenLineSize-1) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("unix", victim.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, _ = io.WriteString(conn, prefix+initialize+call)
			response, err := io.ReadAll(conn)
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("unauthenticated connection was not rejected promptly")
			}
			if len(response) != 0 || checks.Load() != 0 || calls.Load() != 0 {
				t.Fatalf("unauthenticated connection reached victim: bytes=%d checks=%d calls=%d", len(response), checks.Load(), calls.Load())
			}
		})
	}
	var output bytes.Buffer
	if err := ServeStdio(context.Background(), victim.SocketPath, attacker.TokenPath, strings.NewReader(initialize+call), &output); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("foreign stdio credential: %v", err)
	}
	if output.Len() != 0 || checks.Load() != 0 || calls.Load() != 0 {
		t.Fatal("foreign stdio credential leaked data or invoked authority")
	}
	c := newClient(t, victim)
	if !strings.Contains(responseText(c.call("2", `{"text":"authorized"}`)), "victim-private-fixture") || checks.Load() != 1 || calls.Load() != 1 {
		t.Fatal("positive control did not reach victim through its own credential")
	}
	// Authentication applies independently to every connection, even after a
	// legitimate client established a session on the same bridge.
	if err := ServeStdio(context.Background(), victim.SocketPath, attacker.TokenPath, strings.NewReader(initialize+call), io.Discard); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("authenticated connection authorized a different connection: %v", err)
	}
	if checks.Load() != 1 || calls.Load() != 1 {
		t.Fatal("foreign connection reached callbacks after legitimate authentication")
	}
}

func TestBridgeTokenLifetimeAndPermissions(t *testing.T) {
	parent, err := os.MkdirTemp("/tmp", "bridge-auth-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	b := startTest(t, Options{RuntimeDir: parent})
	if filepath.Dir(b.directory) != parent || b.TokenPath != filepath.Join(b.directory, "auth.token") {
		t.Fatal("bridge did not use its host-owned runtime directory")
	}
	token, err := readToken(b.TokenPath)
	if err != nil || len(token) != tokenLineSize {
		t.Fatal("missing or non-private invocation token")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.TokenPath); !os.IsNotExist(err) {
		t.Fatal("invocation token survived bridge close")
	}
}

func TestStdioRejectsInvalidTokenFiles(t *testing.T) {
	b := startTest(t, Options{})
	token, err := readToken(b.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "symlink", "public", "oversized", "malformed"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if name == "symlink" {
				if err := os.Symlink(b.TokenPath, path); err != nil {
					t.Fatal(err)
				}
			} else if name != "missing" {
				data, mode := token, os.FileMode(0600)
				switch name {
				case "public":
					mode = 0644
				case "oversized":
					data = append(append([]byte{}, token...), 'x')
				case "malformed":
					data = []byte(strings.Repeat("!", tokenLineSize-1) + "\n")
				}
				if err := os.WriteFile(path, data, mode); err != nil {
					t.Fatal(err)
				}
			}
			if err := ServeStdio(context.Background(), b.SocketPath, path, strings.NewReader(""), io.Discard); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("accepted %s token: %v", name, err)
			}
		})
	}
}
