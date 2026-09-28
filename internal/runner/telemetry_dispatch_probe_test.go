package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

// These test-only wrappers bracket dispatch preparation at existing runner
// seams. They do not time executor work, process spawn or remote scheduling.
type dispatchProbeKey struct{}

type dispatchProbeStart struct {
	at      time.Time
	run     string
	task    string
	claimed atomic.Bool
}

type dispatchProbeSpans struct{ delegate SpanStarter }

func (p dispatchProbeSpans) StartRun(ctx context.Context, attrs telemetry.RunAttributes) (context.Context, telemetry.Span, error) {
	if p.delegate != nil {
		return p.delegate.StartRun(ctx, attrs)
	}
	return ctx, telemetry.Span{}, nil
}

func (p dispatchProbeSpans) StartGate(ctx context.Context, attrs telemetry.GateAttributes) (context.Context, telemetry.Span, error) {
	if p.delegate != nil {
		return p.delegate.StartGate(ctx, attrs)
	}
	return ctx, telemetry.Span{}, nil
}

func (p dispatchProbeSpans) StartTask(ctx context.Context, attrs telemetry.TaskAttributes) (context.Context, telemetry.Span, error) {
	start := &dispatchProbeStart{at: time.Now(), run: attrs.RunID, task: attrs.TaskID}
	ctx = context.WithValue(ctx, dispatchProbeKey{}, start)
	if p.delegate != nil {
		return p.delegate.StartTask(ctx, attrs)
	}
	return ctx, telemetry.Span{}, nil
}

type dispatchProbeSamples struct {
	mu     sync.Mutex
	values []time.Duration
	limit  int
}

func (p *dispatchProbeSamples) record(ctx context.Context, env apiv1.InvocationEnvelope) error {
	start, ok := ctx.Value(dispatchProbeKey{}).(*dispatchProbeStart)
	if !ok || start.run != env.RunID || env.TaskID != start.run+":"+start.task {
		return errors.New("dispatch probe: missing or mismatched task boundary")
	}
	elapsed := time.Since(start.at) // Capture before sample-lock contention.
	if !start.claimed.CompareAndSwap(false, true) {
		return errors.New("dispatch probe: duplicate executor entry")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.values) >= p.limit {
		return errors.New("dispatch probe: bounded sample capacity exceeded")
	}
	p.values = append(p.values, elapsed)
	return nil
}

type dispatchProbeExecutor struct {
	samples *dispatchProbeSamples
	work    func()
}

func (p dispatchProbeExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if err := p.samples.record(ctx, env); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if p.work != nil {
		p.work()
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func TestRunnerTelemetryDispatchProbeExcludesExecutorWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, _, _ := (dispatchProbeSpans{}).StartTask(context.Background(), telemetry.TaskAttributes{RunID: "run", TaskID: "stage"})
		time.Sleep(2 * time.Second)
		samples := &dispatchProbeSamples{limit: 1}
		executor := dispatchProbeExecutor{samples: samples, work: func() { time.Sleep(5 * time.Second) }}
		env := apiv1.InvocationEnvelope{RunID: "run", TaskID: "run:stage"}
		if _, err := executor.Run(ctx, env, apiv1.DeterministicRun{}); err != nil {
			t.Fatal(err)
		}
		if len(samples.values) != 1 || samples.values[0] != 2*time.Second {
			t.Fatalf("dispatch samples include executor work: %v", samples.values)
		}
		if _, err := executor.Run(ctx, env, apiv1.DeterministicRun{}); err == nil {
			t.Fatal("duplicate executor entry accepted")
		}
	})
}

func TestRunnerTelemetryDispatchProbeRejectsInvalidBoundaries(t *testing.T) {
	samples := &dispatchProbeSamples{limit: 1}
	env := apiv1.InvocationEnvelope{RunID: "run", TaskID: "run:stage"}
	if err := samples.record(context.Background(), env); err == nil {
		t.Fatal("missing task boundary accepted")
	}
	ctx, _, _ := (dispatchProbeSpans{}).StartTask(context.Background(), telemetry.TaskAttributes{RunID: "other", TaskID: "stage"})
	if err := samples.record(ctx, env); err == nil {
		t.Fatal("mismatched task boundary accepted")
	}
	for i := range 2 {
		ctx, _, _ = (dispatchProbeSpans{}).StartTask(context.Background(), telemetry.TaskAttributes{RunID: "run", TaskID: "stage"})
		err := samples.record(ctx, env)
		if (err == nil) != (i == 0) {
			t.Fatalf("bounded record %d: %v", i, err)
		}
	}
}

func TestRunnerTelemetryDispatchProbeReceiverStableIdentity(t *testing.T) {
	receiver := &dispatchProbeReceiver{keys: make(map[string]string)}
	for i, id := range []string{strings.Repeat("a", 64), strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		var body bytes.Buffer
		writer := gzip.NewWriter(&body)
		_, err := fmt.Fprintf(writer, `{"data":{"baseData":{"properties":{"goobers.journal.kind":"run","goobers.journal.id":"run","goobers.journal.seq":"1","goobers.telemetry.record_id":%q}}}}`, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		receiver.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", &body))
		missing, duplicates, invalid := receiver.missing([]string{"run:1"})
		if response.Code != http.StatusOK || missing != 0 || duplicates != i || invalid != (i == 2) {
			t.Fatalf("record %d: status=%d missing=%d duplicates=%d invalid=%t", i, response.Code, missing, duplicates, invalid)
		}
	}
}
