package temporaldial

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc"
)

const recordActivityHeartbeatMethod = "/temporal.api.workflowservice.v1.WorkflowService/RecordActivityTaskHeartbeat"

const maxRetainedHeartbeatTasks = 1024

type heartbeatDetails struct {
	Sequence     uint64    `json:"sequence"`
	GeneratedAt  time.Time `json:"generatedAt"`
	LegacySentAt time.Time `json:"sentAt"`
}

type heartbeatRPCDiagnostics struct {
	converter converter.DataConverter
	logf      func(string, ...any)
	now       func() time.Time

	mu           sync.Mutex
	lastAccepted map[string]time.Time
}

func newHeartbeatRPCDiagnostics(dc converter.DataConverter, logf func(string, ...any)) *heartbeatRPCDiagnostics {
	if dc == nil {
		dc = converter.GetDefaultDataConverter()
	}
	return &heartbeatRPCDiagnostics{
		converter:    dc,
		logf:         logf,
		now:          time.Now,
		lastAccepted: make(map[string]time.Time),
	}
}

func (d *heartbeatRPCDiagnostics) interceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if method != recordActivityHeartbeatMethod {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	request, ok := req.(*workflowservice.RecordActivityTaskHeartbeatRequest)
	if !ok {
		return invoker(ctx, method, req, reply, cc, opts...)
	}

	started := d.now().UTC()
	var details heartbeatDetails
	decoded := request.Details != nil && d.converter.FromPayloads(request.Details, &details) == nil
	generatedAt := details.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = details.LegacySentAt
	}
	decoded = decoded && !generatedAt.IsZero()
	if !decoded {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	key := heartbeatTaskKey(request.TaskToken)
	err := invoker(ctx, method, req, reply, cc, opts...)
	completed := d.now().UTC()

	d.mu.Lock()
	previous := d.lastAccepted[key]
	if err == nil {
		if _, exists := d.lastAccepted[key]; !exists && len(d.lastAccepted) >= maxRetainedHeartbeatTasks {
			var oldestKey string
			var oldest time.Time
			for candidate, acceptedAt := range d.lastAccepted {
				if oldestKey == "" || acceptedAt.Before(oldest) {
					oldestKey, oldest = candidate, acceptedAt
				}
			}
			delete(d.lastAccepted, oldestKey)
		}
		d.lastAccepted[key] = completed
	} else {
		delete(d.lastAccepted, key)
	}
	d.mu.Unlock()

	d.logf("temporal activity heartbeat transport task=%s resource=%q sequence=%d generationAt=%s rpcStartedAt=%s schedulingLatency=%s rpcLatency=%s accepted=%t lastAcceptedAt=%s error=%v",
		key, request.ResourceId, details.Sequence, generatedAt.UTC().Format(time.RFC3339Nano), started.Format(time.RFC3339Nano), nonNegativeDuration(started.Sub(generatedAt)),
		nonNegativeDuration(completed.Sub(started)), err == nil, formatDiagnosticTime(previous), err)
	return err
}

func heartbeatTaskKey(token []byte) string {
	sum := sha256.Sum256(token)
	return fmt.Sprintf("%x", sum[:8])
}

func nonNegativeDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func formatDiagnosticTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
