//go:build cloudintegration

package childengine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestQualificationHeldStageHeartbeatsAndCancels(t *testing.T) {
	for _, loss := range []bool{false, true} {
		name := "heartbeats"
		if loss {
			name = "explicit-heartbeat-loss"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h := &qualificationHarness{t: t}
			server := httptest.NewServer(http.HandlerFunc(h.serveHTTP))
			defer server.Close()
			b := h.StageBarrier("held")
			defer b.Release()
			var beats atomic.Int32
			var duringBarrier atomic.Bool
			observe := func() {
				if beats.Add(1) >= 2 {
					select {
					case <-b.arrived:
						duringBarrier.Store(true)
					default:
					}
					cancel()
				}
			}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.SetWorkerOptions(worker.Options{BackgroundActivityContext: ctx, Interceptors: []interceptor.WorkerInterceptor{&qualificationHeartbeatObserver{observe: observe}}})
			env.SetTestTimeout(5 * time.Second)
			stages := qualificationStages{cfg: qualificationWorkerConfig{Endpoint: server.URL, Script: qualificationStageScript{"child": {Barrier: "held", HeartbeatLoss: loss}}}}
			env.RegisterActivityWithOptions(stages.child, activity.RegisterOptions{Name: "held-child"})
			// The loss case deliberately cannot receive a heartbeat reply; cancel
			// its activity context explicitly to prove the blocked HTTP call exits.
			timer := time.AfterFunc(1500*time.Millisecond, cancel)
			defer timer.Stop()
			_, err := env.ExecuteActivity("held-child", apiv1.InvocationEnvelope{}, apiv1.DeterministicRun{}, "", "")
			if !temporal.IsCanceledError(err) {
				t.Fatalf("held activity did not deliver cancellation: %v", err)
			}
			if loss && beats.Load() != 0 {
				t.Fatalf("heartbeat-loss script emitted %d heartbeats", beats.Load())
			}
			if !loss && (beats.Load() < 2 || !duringBarrier.Load()) {
				t.Fatalf("no periodic heartbeat during held stage: %d", beats.Load())
			}
		})
	}
}

// Observe generation before SDK batching; the production worker separately
// caps heartbeat RPC throttling at engine.DispatchHeartbeatThrottleInterval.
type qualificationHeartbeatObserver struct {
	interceptor.WorkerInterceptorBase
	observe func()
}

func (o *qualificationHeartbeatObserver) InterceptActivity(_ context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &qualificationHeartbeatInbound{ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next}, observe: o.observe}
}

type qualificationHeartbeatInbound struct {
	interceptor.ActivityInboundInterceptorBase
	observe func()
}

func (o *qualificationHeartbeatInbound) Init(next interceptor.ActivityOutboundInterceptor) error {
	return o.Next.Init(&qualificationHeartbeatOutbound{ActivityOutboundInterceptorBase: interceptor.ActivityOutboundInterceptorBase{Next: next}, observe: o.observe})
}

type qualificationHeartbeatOutbound struct {
	interceptor.ActivityOutboundInterceptorBase
	observe func()
}

func (o *qualificationHeartbeatOutbound) RecordHeartbeat(ctx context.Context, details ...any) {
	o.observe()
	o.Next.RecordHeartbeat(ctx, details...)
}
