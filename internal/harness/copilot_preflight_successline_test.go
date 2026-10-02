package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// slowExitProbeArg re-executes this test binary as a Copilot CLI stand-in
// whose sign-in probe prints its reply and then keeps running, like a real CLI
// tearing down its session after answering (#5165).
const slowExitProbeArg = "--goobers-test-slow-exit-probe"

const slowExitReplyPrefix = "--goobers-test-reply="

func runSlowExitProbeFixture() {
	reply := ""
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			_, _ = fmt.Fprintln(os.Stdout, "copilot fixture version")
			os.Exit(0)
		}
		if value, ok := strings.CutPrefix(arg, slowExitReplyPrefix); ok {
			reply = value
		}
	}
	_, _ = fmt.Fprintln(os.Stdout, reply)
	time.Sleep(10 * time.Minute)
	os.Exit(0)
}

func slowExitProbeAdapter(t *testing.T, reply string) *CopilotAdapter {
	t.Helper()
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &CopilotAdapter{
		Command:              []string{program, slowExitProbeArg, slowExitReplyPrefix + reply},
		AuthCheckArgs:        []string{"-p", "Reply with exactly: ok"},
		AuthCheckSuccessLine: "ok",
	}
}

// A probe that has already printed its success reply must pass at once, not
// wait for the CLI to exit and run into the preflight deadline (#5165).
func TestCopilotAdapterPreflightReturnsOnSuccessLineBeforeExit(t *testing.T) {
	adapter := slowExitProbeAdapter(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	info, err := adapter.Preflight(ctx)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if info.Version != "copilot fixture version" {
		t.Fatalf("Preflight version = %q", info.Version)
	}
	// Generous bound: the fixture sleeps 10 minutes after replying, so only
	// an early return can finish anywhere near this.
	if elapsed := time.Since(start); elapsed > time.Minute {
		t.Fatalf("Preflight took %s; it should return as soon as the reply arrives", elapsed)
	}
}

// Without the success line the probe still waits for the process, and a real
// deadline is reported as a timeout rather than an authentication failure.
func TestCopilotAdapterPreflightWithoutSuccessLineReportsTimeout(t *testing.T) {
	adapter := slowExitProbeAdapter(t, "something else")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := adapter.Preflight(ctx)
	if err == nil {
		t.Fatal("expected the probe to time out")
	}
	if !errors.Is(err, ErrTimeout) || strings.Contains(err.Error(), "authentication failure") {
		t.Fatalf("error = %v, want a typed timeout that does not suggest an auth failure", err)
	}
}

// Only the built-in prompt probe may be cut short: a launcher's declared probe
// and adapter-managed session verification must still wait for exit.
func TestAuthProbeSuccessLineGating(t *testing.T) {
	c := &CopilotAdapter{AuthCheckSuccessLine: "ok"}
	builtIn := launcherContract{Version: 1, SessionMode: "adapter-managed"}
	if got := c.authProbeSuccessLine(builtIn, false); got != "ok" {
		t.Fatalf("built-in probe success line = %q, want %q", got, "ok")
	}
	if got := c.authProbeSuccessLine(builtIn, true); got != "" {
		t.Fatalf("session-verifying probe success line = %q, want empty", got)
	}
	declared := launcherContract{Version: 2, SessionMode: "adapter-managed", AuthProbe: &launcherAuthProbe{}}
	if got := c.authProbeSuccessLine(declared, false); got != "" {
		t.Fatalf("launcher-declared probe success line = %q, want empty", got)
	}
}

func TestSuccessLineWatcher(t *testing.T) {
	cases := []struct {
		name   string
		writes []string
		want   bool
	}{
		{"exact line", []string{"ok\n"}, true},
		{"split across writes with CRLF", []string{"o", "k\r", "\n"}, true},
		{"case and surrounding space", []string{"  OK  \n"}, true},
		{"after other output", []string{"thinking...\nok\nusage: 1 request\n"}, true},
		{"longer word", []string{"okay\n"}, false},
		{"embedded in a sentence", []string{"it is ok now\n"}, false},
		{"no terminating newline yet", []string{"ok"}, false},
		{"overlong line ending in ok", []string{strings.Repeat("x", maxSuccessLineBytes+10) + "ok\n"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fired := 0
			w := &successLineWatcher{want: "ok", onSeen: func() { fired++ }}
			for _, chunk := range tc.writes {
				if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			if got := w.seen.Load(); got != tc.want {
				t.Fatalf("seen = %v, want %v", got, tc.want)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.want]; fired != want {
				t.Fatalf("onSeen fired %d times, want %d", fired, want)
			}
		})
	}
	// A repeated success line fires only once.
	fired := 0
	w := &successLineWatcher{want: "ok", onSeen: func() { fired++ }}
	_, _ = w.Write([]byte("ok\nok\n"))
	if fired != 1 {
		t.Fatalf("onSeen fired %d times for a repeated line, want 1", fired)
	}
}
