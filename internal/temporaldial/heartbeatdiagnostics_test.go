package temporaldial

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc"
)

func TestHeartbeatRPCDiagnosticsDistinguishGenerationTransportAndAcceptance(t *testing.T) {
	var logs []string
	diagnostics := newHeartbeatRPCDiagnostics(nil, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	generated := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	times := []time.Time{
		generated.Add(8 * time.Second),
		generated.Add(8*time.Second + 125*time.Millisecond),
		generated.Add(16 * time.Second),
		generated.Add(16*time.Second + 250*time.Millisecond),
	}
	diagnostics.now = func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	}
	payloads, err := converter.GetDefaultDataConverter().ToPayloads(heartbeatDetails{Sequence: 7, GeneratedAt: generated})
	if err != nil {
		t.Fatal(err)
	}
	request := &workflowservice.RecordActivityTaskHeartbeatRequest{TaskToken: []byte("opaque-token"), Details: payloads, ResourceId: "dispatch-run"}
	accept := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error { return nil }
	if err := diagnostics.interceptor(context.Background(), recordActivityHeartbeatMethod, request, &workflowservice.RecordActivityTaskHeartbeatResponse{}, nil, accept); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("activity already timed out")
	reject := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error { return refused }
	if err := diagnostics.interceptor(context.Background(), recordActivityHeartbeatMethod, request, &workflowservice.RecordActivityTaskHeartbeatResponse{}, nil, reject); !errors.Is(err, refused) {
		t.Fatalf("refusal = %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("diagnostic count = %d, want 2", len(logs))
	}
	for _, want := range []string{`resource="dispatch-run"`, "sequence=7", "generationAt=2026-10-16T12:00:00Z", "schedulingLatency=8s", "rpcLatency=125ms", "accepted=true"} {
		if !strings.Contains(logs[0], want) {
			t.Fatalf("accepted diagnostic %q missing %q", logs[0], want)
		}
	}
	for _, want := range []string{"rpcLatency=250ms", "accepted=false", "lastAcceptedAt=2026-10-16T12:00:08.125Z", "activity already timed out"} {
		if !strings.Contains(logs[1], want) {
			t.Fatalf("refused diagnostic %q missing %q", logs[1], want)
		}
	}
}

func TestHeartbeatRPCDiagnosticsIgnoreOtherRPCs(t *testing.T) {
	called := false
	diagnostics := newHeartbeatRPCDiagnostics(nil, func(string, ...any) {
		t.Fatal("non-heartbeat RPC was logged")
	})
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		called = true
		return nil
	}
	if err := diagnostics.interceptor(context.Background(), "/other", struct{}{}, struct{}{}, nil, invoker); err != nil || !called {
		t.Fatalf("pass-through: called=%t err=%v", called, err)
	}
}
