package harness

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	copilot "github.com/github/copilot-sdk/go"
)

// A finished controlled Copilot run must release its SDK session. The SDK
// starts one event-dispatch goroutine per session and only Disconnect stops
// it; Client.ForceStop forgets the session without stopping that goroutine.
// A leaked goroutine pins the session, and through it the transcript buffer
// (the unsubscribed On handler stays in the handler slice's backing array)
// and the RunRequest captured by the permission handler, which reaches the
// executor and its compiled schema validator. A daemon running Copilot lanes
// grew by ~100-150 MB/h this way.
func TestCopilotControlledRunReleasesSession(t *testing.T) {
	const runs = 3
	sessions := make([]weak.Pointer[copilot.Session], 0, runs)
	requests := make([]weak.Pointer[[1 << 16]byte], 0, runs)
	for i := range runs {
		session, request := runControlledCopilotOnce(t, i)
		sessions = append(sessions, session)
		requests = append(requests, request)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		live := 0
		for i := range sessions {
			if sessions[i].Value() != nil || requests[i].Value() != nil {
				live++
			}
		}
		if live == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d closed controlled Copilot runs still reachable: SDK session or its request-scoped state was not released", live, runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runControlledCopilotOnce drives one production controlled run against the
// protocol fixture and returns weak references to the SDK session and to
// request-scoped state its handlers capture. Nothing else outlives it.
func runControlledCopilotOnce(t *testing.T, i int) (weak.Pointer[copilot.Session], weak.Pointer[[1 << 16]byte]) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	var sends atomic.Int32
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		done <- serveCopilotReadinessFixture(conn, &sends, "success")
	}()
	process := &fakeProcessRunner{act: func(req ProcessRequest) error {
		_, err := fmt.Fprintf(req.StdoutCapture, "listening on port %d\n", listener.Addr().(*net.TCPAddr).Port)
		return err
	}}
	base := copilotHeadlessArgvGuard{next: &readinessProtocolProcess{process: process}}
	req := RunRequest{Workspace: t.TempDir(), Tools: goobersIOAvailableToolNames(), GoobersIORegistered: true}
	req.Envelope = testEnvelope(req.Workspace)
	// Stands in for the executor state a real request carries (its
	// completion validator closes over the executor and its schemas).
	executorState := new([1 << 16]byte)
	req.ValidateCompletion = func([]byte) error {
		if executorState[0] != 0 {
			return fmt.Errorf("unexpected state")
		}
		return nil
	}
	config, err := goobersIOAdditionalMCPConfigArg(req, "/test/goobers")
	if err != nil {
		t.Fatal(err)
	}
	runner := &copilotControlledRunner{base: base, request: req, promptIndex: 1, mcpConfig: config}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = runner.Run(ctx, ProcessRequest{Stdin: []byte("prompt " + strings.Repeat("x", i)), Command: []string{"copilot", "-p=", "--session-id", "owned-test-session", "--allow-all-tools"}, Dir: req.Workspace, Env: baseEnv(nil, nil), StdoutCapture: newTranscriptBuffer(8192), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeControlledCopilot(ctx, runner); err != nil {
		t.Fatal(err)
	}
	session, ok := runner.session.(sdkRequiredMCPSession)
	if !ok || session.session == nil {
		t.Fatalf("controlled run did not open an SDK session: %T", runner.session)
	}
	sessionRef := weak.Make(session.session)
	runner.close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return sessionRef, weak.Make(executorState)
}
