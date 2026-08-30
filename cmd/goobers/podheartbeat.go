package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
)

// podheartbeat.go is the mode-3 pod's half of stage liveness (#3875, decision
// 005 / finding 002 plan item E3).
//
// The local runner emits a stage.heartbeat every StageHeartbeatInterval that a
// stage attempt makes observable progress (internal/runner.startStageHeartbeat),
// and the daemon's stalled-run sweep reads the last journal event as its ONLY
// liveness signal for a long stage (cmd/goobers/stalledruns.go). A pod stage
// emitted nothing at all between its stage.started and its own completion, so a
// 70-minute `make ci` or a long agent turn in a pod was indistinguishable from a
// hang and the blunt stalledRunTimeout pin was the only margin.
//
// This closes that gap on the same terms the runner sets, deliberately and in
// every particular:
//
//   - SAME INTERVAL (runner.StageHeartbeatInterval), read from the runner's own
//     constant rather than restated, so the sweep's thresholds mean one thing
//     across substrates.
//   - SAME PROGRESS GATE. The runner emits on a tick ONLY if the executor
//     reported progress since the previous one, so a stage that has genuinely
//     gone silent still goes silent in the journal and the sweep still catches
//     it. A pod that heartbeat unconditionally would be a pod that can never be
//     swept, which is a worse bug than the one being fixed: it would convert
//     every wedged pod stage into an eternal one.
//   - SAME TERMINATOR. The ticker goroutine selects on its stop channel and
//     NOT on ctx.Done(), for the reason #3455 records on the runner's copy: a
//     drain cancels the context and then waits out the termination grace period
//     while the stage keeps running, and a heartbeat that stopped at the instant
//     of cancellation would go quiet for exactly the window in which proving
//     liveness matters most. Stop() is called on every exit path from the stage.
//
// The route is the journal plane (POST /api/v1/runs/{run}/journal/emit), the
// same one the pod already uses for its stage artifacts — there is no local
// journal in a pod, and adding a second transport for four bytes of liveness
// would be a second thing to get wrong. stage.heartbeat is conformance-EXCLUDED
// (internal/journal/event.go), so these appends cannot diverge the DS5 live-vs-
// projected cross-check and cannot affect replay: the workflow never learns they
// happened.

// podStageHeartbeatInterval is how often the pod's heartbeat goroutine wakes.
//
// A var bound to the runner's constant, not a copy of its value: the binding is
// what makes "the shared StageHeartbeatInterval" true rather than aspirational,
// and the var exists only so a test can observe several ticks in bounded time
// (the same seam runner.Config's heartbeatInterval provides on the other side).
var podStageHeartbeatInterval = runner.StageHeartbeatInterval

// podHeartbeatEmitTimeout bounds ONE heartbeat emission. Short on purpose: a
// heartbeat is worthless late, the next tick supersedes it, and a hung journal
// plane must never accumulate goroutines inside a pod whose real job is the
// stage.
const podHeartbeatEmitTimeout = 30 * time.Second

// podStageHeartbeat is the handle Stop() is called on. The zero value is a
// no-op handle, which is what a pod with no journal plane to emit to gets.
type podStageHeartbeat struct {
	stop chan struct{}
	done <-chan struct{}
}

// Stop ends the heartbeat and WAITS for its goroutine to exit.
//
// The wait is the no-leak contract, and it is why done is closed by the
// goroutine's own defer rather than by Stop: when Stop returns, the goroutine is
// gone and no further emission can be in flight. A pod process that exited while
// a heartbeat was still being written would race its own surrender PUT.
//
// Idempotent-by-construction for the zero handle (nothing to close), so callers
// can `defer heartbeat.Stop()` without first testing whether one started.
func (h podStageHeartbeat) Stop() {
	if h.stop == nil {
		return
	}
	close(h.stop)
	<-h.done
}

// podStageIdentity is the pod's own answer to "which attempt of which stage of
// which run am I?", read from the environment podspec.go stamps.
type podStageIdentity struct {
	daemonAPI string
	token     string
	runID     string
	gaggle    string
	stage     string
	attempt   int
}

// podStageIdentityFromEnv reads the identity, reporting false when this pod has
// no journal plane to emit into — the loopback/no-plane posture
// recordStageArtifacts already treats as a silent no-op.
func podStageIdentityFromEnv() (podStageIdentity, bool) {
	id := podStageIdentity{
		daemonAPI: strings.TrimSpace(os.Getenv(dispatcher.EnvDaemonAPI)),
		token:     os.Getenv(dispatcher.EnvPodToken),
		runID:     os.Getenv(dispatcher.EnvRunID),
		gaggle:    os.Getenv(dispatcher.EnvGaggle),
		stage:     os.Getenv(dispatcher.EnvStage),
		attempt:   1,
	}
	if attempt, err := strconv.Atoi(os.Getenv(dispatcher.EnvAttempt)); err == nil && attempt >= 1 {
		id.attempt = attempt
	}
	if id.daemonAPI == "" || id.runID == "" || id.stage == "" {
		return podStageIdentity{}, false
	}
	return id, true
}

// startPodStageHeartbeat installs the progress reporter on ctx and starts the
// heartbeat goroutine. The returned context is what the stage must run under:
// without it nothing reports progress and every tick is skipped.
//
// Both pod stage kinds are covered by that one seam. An AGENTIC stage's harness
// already calls invoke.ReportProgress on every transcript write
// (internal/harness/process.go), exactly as it does on the local runner. A
// DETERMINISTIC stage's command is exec'd directly by dispatchexec.go rather
// than through internal/executor, so its output writers report progress through
// podStageProgressWriter — one mechanism, two attachment points, which is why
// the interval and the silence rule are identical for both.
func startPodStageHeartbeat(ctx context.Context, stderr io.Writer) (context.Context, podStageHeartbeat) {
	id, ok := podStageIdentityFromEnv()
	if !ok {
		return ctx, podStageHeartbeat{}
	}
	emitter := &livejournal.HTTPEmitter{BaseURL: id.daemonAPI, Token: id.token}
	return startPodStageHeartbeatWith(ctx, stderr, id, emitter)
}

// podJournalEmitter is the slice of livejournal.HTTPEmitter the heartbeat uses,
// declared as an interface so the timing and cancellation tests can drive the
// loop without an HTTP server standing in for the thing under test.
type podJournalEmitter interface {
	Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error)
}

func startPodStageHeartbeatWith(ctx context.Context, stderr io.Writer, id podStageIdentity, emitter podJournalEmitter) (context.Context, podStageHeartbeat) {
	stop := make(chan struct{})
	done := make(chan struct{})
	var progressed atomic.Bool
	ctx = invoke.WithProgressReporter(ctx, func() { progressed.Store(true) })

	// The EMIT context is deliberately not the stage's. On SIGTERM (pod
	// deletion, eviction, node drain) the caller's signal context is already
	// cancelled while the stage runs on through its termination grace period —
	// the very window the heartbeat exists to cover — so every emission would
	// fail instantly with "context canceled". Same position, same reason, as
	// stageBlobWriteThroughContext.
	emitCtx := context.WithoutCancel(ctx)
	ticker := time.NewTicker(podStageHeartbeatInterval)
	go func() {
		defer close(done)
		defer ticker.Stop()
		for tick := 0; ; {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !progressed.Swap(false) {
					continue
				}
				tick++
				emitPodStageHeartbeat(emitCtx, stderr, id, emitter, tick)
			}
		}
	}()
	return ctx, podStageHeartbeat{stop: stop, done: done}
}

// emitPodStageHeartbeat posts one stage.heartbeat.
//
// BEST EFFORT, like every other pod-side journal emission: the stage's own
// result is the authoritative outcome and a journal plane that is briefly down
// must not convert a running stage into a failed one. A refusal is reported on
// the pod's stderr so `kubectl logs` shows it, and the next tick tries again.
//
// The idempotency key is (stage, attempt, ordinal), which is distinct across
// ticks and across attempts — a redelivered tick deduplicates, and a retried
// attempt's first heartbeat is never mistaken for the previous attempt's.
func emitPodStageHeartbeat(ctx context.Context, stderr io.Writer, id podStageIdentity, emitter podJournalEmitter, tick int) {
	emitCtx, cancel := context.WithTimeout(ctx, podHeartbeatEmitTimeout)
	defer cancel()
	_, err := emitter.Emit(emitCtx, livejournal.EmitRequest{
		RunID:  id.runID,
		Gaggle: id.gaggle,
		Ops: []livejournal.Op{{
			Kind: livejournal.OpAppend,
			Key:  fmt.Sprintf("%s/%d/heartbeat/%d", id.stage, id.attempt, tick),
			// The daemon's replayClock adopts this verbatim; an unstamped op
			// persists at 0001-01-01T00:00:00Z (#3774), which for a LIVENESS
			// event would drag the run's LastActivity to the zero instant and
			// make the sweep escalate the very stage this proves is alive.
			Time: time.Now().UTC(),
			Event: &journal.Event{
				Type:    journal.EventStageHeartbeat,
				Stage:   id.stage,
				Attempt: id.attempt,
			},
		}},
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dispatch-exec: emit stage heartbeat: %v\n", err)
	}
}

// podStageProgressWriter reports observable progress for a deterministic pod
// stage: every non-empty write the stage's command makes to stdout or stderr is
// one progress signal, coalesced by the heartbeat's ticker into at most one
// event per interval.
//
// This is the pod's equivalent of internal/executor/shell.go's capturingWriter
// progress hook, and it is attached in the same place — the MultiWriter the
// command's streams already fan out through — so a stage produces heartbeats on
// a pod for exactly the output that produces them on a self runner.
type podStageProgressWriter struct {
	report func()
}

func (w podStageProgressWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && w.report != nil {
		w.report()
	}
	return len(p), nil
}

// podStageProgress returns the writer that feeds ctx's progress reporter. It is
// always non-nil and always safe to attach: invoke.ReportProgress is a no-op on
// a context carrying no reporter, which is the shape a pod with no journal plane
// (and every direct unit-test call into runDeclaredStage) runs under.
func podStageProgress(ctx context.Context) io.Writer {
	return podStageProgressWriter{report: func() { invoke.ReportProgress(ctx) }}
}
