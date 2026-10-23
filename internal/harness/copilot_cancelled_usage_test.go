package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

// The fixture's endpoint dies with its owned process context, like the native
// CLI. Older tests kept the socket alive independently and hid the race.
func TestCopilotCancelledSessionCapturesUsageBeforeTeardown(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "unavailable", "stalled"} {
		t.Run(mode, func(t *testing.T) { testCopilotCancelledAccounting(t, mode) })
	}
}

func testCopilotCancelledAccounting(t *testing.T, mode string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if mode == "deadline" {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithTimeout(ctx, time.Second)
		defer deadlineCancel()
	}
	usageStarted, releaseUsage, processExited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUsage) }) }
	defer release()
	var mu sync.Mutex
	var calls []string
	options := copilotReadinessFixtureOptions{
		onRPC: func(method string) { mu.Lock(); calls = append(calls, method); mu.Unlock() },
		afterSend: func() {
			if mode != "deadline" {
				cancel()
			}
		},
		beforeUsage: func() error {
			close(usageStarted)
			if mode == "unavailable" {
				return io.EOF
			}
			if mode == "stalled" {
				<-processExited
				return io.EOF
			}
			<-releaseUsage
			return nil
		},
	}
	runner, req, fixtureDone := cancelledCopilotFixture(t, options, processExited)
	defer runner.close()
	result := make(chan error, 1)
	go func() { _, err := runner.Run(ctx, req); result <- err }()
	select {
	case <-usageStarted:
	case err := <-result:
		t.Fatalf("exited before usage RPC: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("did not reach cancellation accounting")
	}
	if mode == "cancel" || mode == "deadline" {
		select {
		case <-processExited:
			t.Fatal("process torn down before usage reply")
		default:
		}
		mu.Lock()
		aborted := slices.Contains(calls, "session.abort")
		mu.Unlock()
		if !aborted {
			t.Fatal("usage queried before aborting the cancelled prompt")
		}
		release()
	}
	var runErr error
	select {
	case runErr = <-result:
	case <-time.After(copilotFinalizeTimeout + 3*time.Second):
		t.Fatal("cancelled finalization did not stay bounded")
	}
	wantErr := ErrCanceled
	if mode == "deadline" {
		wantErr = ErrTimeout
	}
	if !errors.Is(runErr, wantErr) {
		t.Fatalf("cancellation classification lost: %v", runErr)
	}
	select {
	case <-processExited:
	default:
		t.Fatal("owned process survived Run")
	}
	if err := finalizeControlledCopilot(ctx, runner); err != nil {
		t.Fatalf("duplicate finalization: %v", err)
	}
	var out Outcome
	applyControlledCopilotUsage(&out, runner)
	applyControlledCopilotUsage(&out, runner)
	if mode == "cancel" || mode == "deadline" {
		if out.Metrics[telemetry.AttrGenAIUsageInputTokens] != 42 || out.Metrics[telemetry.AttrUsageNanoAIU] != 1e9 || len(out.ModelUsage) != 1 {
			t.Fatalf("lost or double-counted usage: %+v", out)
		}
	} else if !strings.Contains(runErr.Error(), "usage unknown") || len(out.Metrics) != 0 {
		t.Fatalf("missing usage misrepresented as measured: err=%v metrics=%v", runErr, out.Metrics)
	}
	mu.Lock()
	defer mu.Unlock()
	if countRPC(calls, "session.abort") != 1 || countRPC(calls, "session.usage.getMetrics") != 1 {
		t.Fatalf("repeated/missing accounting: %v", calls)
	}
	if mode == "cancel" || mode == "deadline" {
		if countRPC(calls, "session.shutdown") != 1 {
			t.Fatalf("missing shutdown: %v", calls)
		}
	}
	select {
	case <-fixtureDone:
	case <-time.After(time.Second):
		t.Fatal("fixture endpoint survived cleanup")
	}
}

func countRPC(calls []string, method string) int {
	count := 0
	for _, call := range calls {
		if call == method {
			count++
		}
	}
	return count
}

func cancelledCopilotFixture(t *testing.T, options copilotReadinessFixtureOptions, processExited chan struct{}) (*copilotControlledRunner, ProcessRequest, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	connection := make(chan net.Conn, 1)
	fixtureDone := make(chan struct{})
	var sends atomic.Int32
	go func() {
		defer close(fixtureDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		connection <- conn
		_ = serveCopilotReadinessFixtureWithOptions(conn, &sends, "success", options)
	}()
	process := launcherProcessRunner(func(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
		defer close(processExited)
		if _, err := fmt.Fprintf(req.StdoutCapture, "listening on port %d\n", listener.Addr().(*net.TCPAddr).Port); err != nil {
			return ProcessResult{}, err
		}
		<-ctx.Done()
		select {
		case conn := <-connection:
			_ = conn.Close()
		default:
		}
		_ = listener.Close()
		return ProcessResult{}, ctx.Err()
	})
	request := RunRequest{Workspace: t.TempDir(), Tools: goobersIOAvailableToolNames(), GoobersIORegistered: true}
	request.Envelope = testEnvelope(request.Workspace)
	config, err := goobersIOAdditionalMCPConfigArg(request, "/test/goobers")
	if err != nil {
		t.Fatal(err)
	}
	runner := &copilotControlledRunner{base: process, request: request, promptIndex: 1, mcpConfig: config}
	req := ProcessRequest{Stdin: []byte("cancelled fixture prompt"), Command: []string{"copilot", "-p=", "--session-id", "owned-test-session", "--allow-all-tools"}, Dir: request.Workspace, Env: baseEnv(nil, nil), Timeout: 10 * time.Second}
	return runner, req, fixtureDone
}
