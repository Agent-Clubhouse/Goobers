package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
)

// podheartbeat_test.go covers plan item E3's pod half (#3875): a stage running
// in a dispatcher-created pod emits stage.heartbeat through the journal plane on
// the runner's own interval, so the daemon's stall sweep has the same liveness
// signal on both substrates.
//
// The failure this pins is a silent one and cannot be caught downstream: without
// these events a pod stage's journal has NOTHING between stage.started and its
// own completion, so a 70-minute `make ci` and a wedged pod are the same
// observation and the blunt stalledRunTimeout pin is the only margin.

// recordingJournalEmitter captures emitted batches. It stands in for
// livejournal.HTTPEmitter so the tests drive the heartbeat loop itself rather
// than an HTTP server's timing.
type recordingJournalEmitter struct {
	mu       sync.Mutex
	requests []livejournal.EmitRequest
	err      error
	emitted  chan struct{}
}

func newRecordingJournalEmitter() *recordingJournalEmitter {
	return &recordingJournalEmitter{emitted: make(chan struct{}, 64)}
}

func (e *recordingJournalEmitter) Emit(_ context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.mu.Lock()
	e.requests = append(e.requests, req)
	err := e.err
	e.mu.Unlock()
	select {
	case e.emitted <- struct{}{}:
	default:
	}
	return livejournal.EmitResponse{}, err
}

func (e *recordingJournalEmitter) recorded() []livejournal.EmitRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]livejournal.EmitRequest(nil), e.requests...)
}

// awaitEmissions waits for at least n batches, failing rather than hanging.
func (e *recordingJournalEmitter) awaitEmissions(t *testing.T, n int) []livejournal.EmitRequest {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if got := e.recorded(); len(got) >= n {
			return got
		}
		select {
		case <-e.emitted:
		case <-deadline:
			t.Fatalf("waited for %d heartbeat emission(s), saw %d", n, len(e.recorded()))
		}
	}
}

func testPodStageIdentity() podStageIdentity {
	return podStageIdentity{
		daemonAPI: "http://daemon.invalid", token: "pod-token",
		runID: "run-hb", gaggle: "web", stage: "make-ci", attempt: 2,
	}
}

// shortenPodHeartbeatInterval makes the shared interval observable in bounded
// time. The production value is runner.StageHeartbeatInterval and
// TestPodHeartbeatUsesTheSharedRunnerInterval pins that it is the value the
// production path reads.
func shortenPodHeartbeatInterval(t *testing.T) {
	t.Helper()
	previous := podStageHeartbeatInterval
	podStageHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { podStageHeartbeatInterval = previous })
}

// THE INTERVAL IS THE RUNNER'S, not a pod-local copy. The stall sweep's
// thresholds are calibrated against one cadence, and a second constant here is
// how the two substrates would drift apart silently.
func TestPodHeartbeatUsesTheSharedRunnerInterval(t *testing.T) {
	if podStageHeartbeatInterval != runner.StageHeartbeatInterval {
		t.Fatalf("podStageHeartbeatInterval = %v, want runner.StageHeartbeatInterval (%v)",
			podStageHeartbeatInterval, runner.StageHeartbeatInterval)
	}
}

// TIMING: a stage that keeps making progress keeps heartbeating, one event per
// interval, carrying the attempt identity a reader correlates to stage.started.
func TestPodHeartbeatEmitsPerIntervalWhileTheStageProgresses(t *testing.T) {
	shortenPodHeartbeatInterval(t)
	emitter := newRecordingJournalEmitter()
	id := testPodStageIdentity()

	ctx, heartbeat := startPodStageHeartbeatWith(context.Background(), io.Discard, id, emitter)
	progressing := make(chan struct{})
	go func() {
		for {
			select {
			case <-progressing:
				return
			default:
				invoke.ReportProgress(ctx)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	requests := emitter.awaitEmissions(t, 3)
	close(progressing)
	heartbeat.Stop()

	for i, req := range requests {
		if req.RunID != id.runID || req.Gaggle != id.gaggle {
			t.Fatalf("emission %d addressed %s/%s, want %s/%s", i, req.Gaggle, req.RunID, id.gaggle, id.runID)
		}
		if len(req.Ops) != 1 || req.Ops[0].Kind != livejournal.OpAppend || req.Ops[0].Event == nil {
			t.Fatalf("emission %d is not a single append op: %+v", i, req.Ops)
		}
		event := req.Ops[0].Event
		if event.Type != journal.EventStageHeartbeat {
			t.Fatalf("emission %d event type = %q, want %q", i, event.Type, journal.EventStageHeartbeat)
		}
		if event.Stage != id.stage || event.Attempt != id.attempt {
			t.Fatalf("emission %d identifies %s#%d, want %s#%d — a heartbeat a reader cannot attribute to an attempt is not a liveness signal",
				i, event.Stage, event.Attempt, id.stage, id.attempt)
		}
		// An unstamped op persists at the zero instant (#3774), which for a
		// liveness event would drag the run's LastActivity backwards and make
		// the sweep escalate the stage this proves is alive.
		if req.Ops[0].Time.IsZero() {
			t.Fatalf("emission %d carries no op time; the daemon's replay clock would durably record it at 0001-01-01", i)
		}
	}
	// Distinct idempotency keys, or the writer deduplicates every heartbeat
	// after the first and the journal goes quiet exactly as before.
	keys := map[string]bool{}
	for _, req := range requests {
		key := req.Ops[0].Key
		if keys[key] {
			t.Fatalf("heartbeat key %q was reused; the live writer deduplicates by key and would drop it", key)
		}
		keys[key] = true
		if !strings.Contains(key, id.stage) || !strings.Contains(key, "2") {
			t.Fatalf("heartbeat key %q does not scope to the stage attempt; a retried attempt would collide with the previous one", key)
		}
	}
}

// THE SILENCE RULE, and it is the half that makes the mechanism safe: no
// progress means no heartbeat, exactly as on the local runner. A pod that
// heartbeat unconditionally could never be swept — every wedged pod stage would
// become an eternal one, which is worse than the blindness being fixed.
func TestPodHeartbeatStaysSilentWithoutProgress(t *testing.T) {
	shortenPodHeartbeatInterval(t)
	emitter := newRecordingJournalEmitter()

	_, heartbeat := startPodStageHeartbeatWith(context.Background(), io.Discard, testPodStageIdentity(), emitter)
	time.Sleep(100 * time.Millisecond) // ~20 ticks at the shortened interval
	heartbeat.Stop()

	if got := emitter.recorded(); len(got) != 0 {
		t.Fatalf("a stage that reported no progress emitted %d heartbeat(s); the stall sweep must still be able to catch a wedged pod", len(got))
	}
}

// CANCELLATION: the drain case #3455 records on the runner's copy. A cancelled
// context means the pod is being terminated and the stage keeps running for its
// termination grace period — the window in which proving liveness matters most —
// so the heartbeat must NOT stop at the instant of cancellation, and its
// emissions must not fail with "context canceled" either.
func TestPodHeartbeatSurvivesContextCancellation(t *testing.T) {
	shortenPodHeartbeatInterval(t)
	emitter := newRecordingJournalEmitter()

	ctx, cancel := context.WithCancel(context.Background())
	stageCtx, heartbeat := startPodStageHeartbeatWith(ctx, io.Discard, testPodStageIdentity(), emitter)
	cancel()

	deadline := time.After(5 * time.Second)
	for len(emitter.recorded()) == 0 {
		invoke.ReportProgress(stageCtx)
		select {
		case <-emitter.emitted:
		case <-time.After(2 * time.Millisecond):
		case <-deadline:
			heartbeat.Stop()
			t.Fatal("no heartbeat after the stage context was cancelled; a drained pod goes journal-silent for the whole termination grace period")
		}
	}
	heartbeat.Stop()

	for _, req := range emitter.recorded() {
		if req.Ops[0].Time.IsZero() {
			t.Fatal("a post-cancellation heartbeat lost its op time")
		}
	}
}

// LIFECYCLE: Stop() ends the goroutine and WAITS for it, so nothing is still
// emitting once the caller moves on to its surrender PUT. Proven by the strongest
// observable form of "the goroutine is gone": no emission can appear after Stop
// returns, however much progress is reported afterwards.
func TestPodHeartbeatStopIsSynchronousAndLeavesNoGoroutine(t *testing.T) {
	shortenPodHeartbeatInterval(t)
	emitter := newRecordingJournalEmitter()

	ctx, heartbeat := startPodStageHeartbeatWith(context.Background(), io.Discard, testPodStageIdentity(), emitter)
	invoke.ReportProgress(ctx)
	emitter.awaitEmissions(t, 1)
	heartbeat.Stop()

	settled := len(emitter.recorded())
	for i := 0; i < 200; i++ {
		invoke.ReportProgress(ctx)
		time.Sleep(time.Millisecond)
	}
	if got := len(emitter.recorded()); got != settled {
		t.Fatalf("%d heartbeat(s) were emitted after Stop returned (was %d); the goroutine outlived its stage and races the surrender PUT", got-settled, settled)
	}
}

// A pod with no journal plane (the loopback/no-plane posture) gets a no-op
// handle, and Stop on it must be safe — callers `defer heartbeat.Stop()` without
// first asking whether one started.
func TestPodHeartbeatNoOpsWithoutAJournalPlane(t *testing.T) {
	t.Setenv(dispatcher.EnvDaemonAPI, "")
	t.Setenv(dispatcher.EnvRunID, "run-hb")
	t.Setenv(dispatcher.EnvStage, "make-ci")
	t.Setenv(dispatcher.EnvAttempt, "1")

	ctx, heartbeat := startPodStageHeartbeat(context.Background(), io.Discard)
	if ctx == nil {
		t.Fatal("startPodStageHeartbeat returned a nil context")
	}
	heartbeat.Stop()
	heartbeat.Stop() // the zero handle must stay safe on a second call
}

// A journal plane that refuses is BEST EFFORT, like every other pod-side
// emission: the stage's own result is authoritative and a brief outage must not
// convert a running stage into a failed one. The refusal is still VISIBLE on the
// pod's stderr, which is what `kubectl logs` shows an operator.
func TestPodHeartbeatEmissionFailureIsVisibleAndNonFatal(t *testing.T) {
	shortenPodHeartbeatInterval(t)
	emitter := newRecordingJournalEmitter()
	emitter.err = errors.New("journal plane down")
	stderr := &syncBuffer{}

	ctx, heartbeat := startPodStageHeartbeatWith(context.Background(), stderr, testPodStageIdentity(), emitter)
	invoke.ReportProgress(ctx)
	emitter.awaitEmissions(t, 1)
	// Let the reporting goroutine finish writing before Stop races the read.
	heartbeat.Stop()

	if !strings.Contains(stderr.String(), "emit stage heartbeat") {
		t.Fatalf("a refused heartbeat must be visible on the pod's own stderr, got %q", stderr.String())
	}
}

// podStageIdentityFromEnv reads what podspec.go stamps, and refuses the shapes
// that have nothing to emit into rather than emitting into the wrong run.
func TestPodStageIdentityFromEnv(t *testing.T) {
	t.Run("complete identity", func(t *testing.T) {
		t.Setenv(dispatcher.EnvDaemonAPI, "http://daemon:7777")
		t.Setenv(dispatcher.EnvPodToken, "tok")
		t.Setenv(dispatcher.EnvRunID, "run-9")
		t.Setenv(dispatcher.EnvGaggle, "web")
		t.Setenv(dispatcher.EnvStage, "build")
		t.Setenv(dispatcher.EnvAttempt, "3")
		id, ok := podStageIdentityFromEnv()
		if !ok || id.runID != "run-9" || id.stage != "build" || id.attempt != 3 || id.gaggle != "web" || id.token != "tok" {
			t.Fatalf("identity = %+v (ok=%t)", id, ok)
		}
	})

	t.Run("unparseable attempt falls back to 1 rather than 0", func(t *testing.T) {
		t.Setenv(dispatcher.EnvDaemonAPI, "http://daemon:7777")
		t.Setenv(dispatcher.EnvRunID, "run-9")
		t.Setenv(dispatcher.EnvStage, "build")
		t.Setenv(dispatcher.EnvAttempt, "")
		id, ok := podStageIdentityFromEnv()
		if !ok || id.attempt != 1 {
			t.Fatalf("identity = %+v (ok=%t), want attempt 1 — attempt 0 matches no stage.started", id, ok)
		}
	})

	for name, missing := range map[string]string{
		"no daemon api": dispatcher.EnvDaemonAPI,
		"no run id":     dispatcher.EnvRunID,
		"no stage":      dispatcher.EnvStage,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(dispatcher.EnvDaemonAPI, "http://daemon:7777")
			t.Setenv(dispatcher.EnvRunID, "run-9")
			t.Setenv(dispatcher.EnvStage, "build")
			t.Setenv(missing, "")
			if _, ok := podStageIdentityFromEnv(); ok {
				t.Fatalf("%s must not produce an emitting identity", name)
			}
		})
	}
}

// The deterministic pod stage's own progress attachment: output IS progress,
// exactly as internal/executor/shell.go's capturingWriter treats it on a self
// runner. Without this a `make ci` in a pod would tick silently forever.
func TestPodStageProgressWriterReportsOnOutput(t *testing.T) {
	var reports int
	w := podStageProgressWriter{report: func() { reports++ }}
	n, err := w.Write([]byte("compiling...\n"))
	if err != nil || n != len("compiling...\n") {
		t.Fatalf("Write = (%d, %v), want the full length and no error — this writer sits in the stage's MultiWriter", n, err)
	}
	if reports != 1 {
		t.Fatalf("reports = %d, want 1", reports)
	}
	if _, err := w.Write(nil); err != nil {
		t.Fatalf("empty write errored: %v", err)
	}
	if reports != 1 {
		t.Fatalf("an empty write reported progress; only real output is evidence of life")
	}
}

// End to end through the real entrypoint: a deterministic stage that produces
// output over more than one interval emits stage.heartbeat into the run's
// journal plane, and the pod still surrenders normally.
func TestDispatchExecHeartbeatsADeterministicStage(t *testing.T) {
	shortenPodHeartbeatInterval(t)
	emitted := make(chan livejournal.EmitRequest, 64)
	server := newJournalAndSurrenderServer(t, emitted)

	t.Setenv(dispatcher.EnvRunID, "run-hb-e2e")
	t.Setenv(dispatcher.EnvGaggle, "web")
	t.Setenv(dispatcher.EnvStage, "make-ci")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvDaemonAPI, server)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","for i in 1 2 3 4 5 6 7 8 9 10; do echo tick $i; sleep 0.02; done"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "30s")

	if code := runDispatchExecContext(context.Background(), io.Discard, &syncBuffer{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var heartbeats int
	for {
		select {
		case req := <-emitted:
			for _, op := range req.Ops {
				if op.Event != nil && op.Event.Type == journal.EventStageHeartbeat {
					if op.Event.Stage != "make-ci" || op.Event.Attempt != 1 {
						t.Fatalf("heartbeat identifies %s#%d", op.Event.Stage, op.Event.Attempt)
					}
					heartbeats++
				}
			}
			continue
		default:
		}
		break
	}
	if heartbeats == 0 {
		t.Fatal("a pod stage producing output over several intervals emitted no stage.heartbeat; the stall sweep is still blind to it")
	}
}

// syncBuffer is a stderr stand-in safe for the heartbeat goroutine and the main
// stage path to share, as os.Stderr is in production.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newJournalAndSurrenderServer stands in for the write API: it accepts the
// journal-plane emits the heartbeat posts, forwarding them to emitted, and
// accepts the surrender PUT the wrapper finishes with.
func newJournalAndSurrenderServer(t *testing.T, emitted chan<- livejournal.EmitRequest) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/journal/emit") {
			var req livejournal.EmitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
				select {
				case emitted <- req:
				default:
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server.URL
}
