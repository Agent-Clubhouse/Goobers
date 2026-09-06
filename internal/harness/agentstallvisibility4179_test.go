package harness

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// #4179: run f5faeec4ee947f88af7a09204db51bcb burned the whole 5400s implement
// budget on a stalled `go mod download`, and the run's own journal could not
// say so — one agent.lifecycle event, no per-turn agent events, and stage
// heartbeats that kept arriving for ninety minutes. From the record, a blocked
// session and a looping one were the same thing, and the reflex was to raise
// the budget a third time.
//
// PR #4444 bounded the download in its own stage (the issue's proposed fix
// item 1). This covers item 3, which nothing addressed: emitting periodic
// activity marks so the distinction is visible from `goobers trace` without
// opening the transcript artifact.

func stallTestRequest() RunRequest {
	return RunRequest{
		Envelope: apiv1.InvocationEnvelope{RunID: "run-4179", TaskID: "implement", Goal: "implement", Attempt: 1},
		Attempt:  1,
	}
}

// collectAgentEvents wires an emitter whose events are captured as the sink
// sees them — i.e. in the order they would reach the live journal, not the
// order they end up in the Outcome.
func collectAgentEvents(t *testing.T) (*adapterAgentEmitter, func() []journal.Event) {
	t.Helper()
	var mu sync.Mutex
	var seen []journal.Event
	req := stallTestRequest()
	req.AgentEventSink = func(event journal.Event) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, event)
		return nil
	}
	emitter, err := beginAdapterAgentTelemetry(req, "copilot", "m", "m", "", "")
	if err != nil {
		t.Fatalf("beginAdapterAgentTelemetry: %v", err)
	}
	return emitter, func() []journal.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]journal.Event(nil), seen...)
	}
}

func lifecycles(events []journal.Event) []journal.AgentLifecycle {
	out := make([]journal.AgentLifecycle, 0, len(events))
	for _, ev := range events {
		if ev.Type == journal.EventAgentLifecycle && ev.Agent != nil {
			out = append(out, ev.Agent.Lifecycle)
		}
	}
	return out
}

// TestStalledSessionIsVisibleInTheJournal is the regression proper: a session
// that produces nothing writes `waiting` marks into the journal, which is the
// evidence #4179's run did not have.
func TestStalledSessionIsVisibleInTheJournal(t *testing.T) {
	emitter, events := collectAgentEvents(t)

	// A tracker that never observes output — the stalled `go mod download`.
	tracker := newActivityTracker(time.Now())
	sampler := startActivitySampler(context.Background(), tracker, time.Millisecond, func() int64 { return 0 }, emitter.activityObserver())
	waitForLifecycle(t, events, journal.AgentWaiting)
	sampler.stop()

	got := lifecycles(events())
	if len(got) == 0 || got[0] != journal.AgentStarted {
		t.Fatalf("lifecycle sequence does not open with started: %v", got)
	}
	var waiting int
	for _, l := range got[1:] {
		switch l {
		case journal.AgentWaiting:
			waiting++
		case journal.AgentResumed:
			t.Errorf("a session that produced no output reported %q", l)
		}
	}
	if waiting == 0 {
		t.Fatal("a stalled session produced no waiting marks: the journal still cannot distinguish blocked from looping (#4179)")
	}
}

// TestProducingSessionReportsResumed is the other half. Without it, "waiting"
// would be indistinguishable from a mark the code emits unconditionally, and
// the signal would be worthless.
func TestProducingSessionReportsResumed(t *testing.T) {
	emitter, events := collectAgentEvents(t)

	tracker := newActivityTracker(time.Now())
	stop := make(chan struct{})
	var writing sync.WaitGroup
	writing.Add(1)
	go func() {
		defer writing.Done()
		for {
			select {
			case <-stop:
				return
			default:
				tracker.observe(time.Now())
				time.Sleep(time.Millisecond)
			}
		}
	}()

	sampler := startActivitySampler(context.Background(), tracker, 5*time.Millisecond, func() int64 { return 1 }, emitter.activityObserver())
	waitForLifecycle(t, events, journal.AgentResumed)
	sampler.stop()
	close(stop)
	writing.Wait()
}

// TestSamplerStopsBeforeTerminalEvent guards the ordering that makes these
// marks safe to read: a liveness mark arriving after the terminal event would
// say a completed agent is still waiting.
func TestSamplerStopsBeforeTerminalEvent(t *testing.T) {
	emitter, events := collectAgentEvents(t)

	tracker := newActivityTracker(time.Now())
	sampler := startActivitySampler(context.Background(), tracker, time.Millisecond, func() int64 { return 0 }, emitter.activityObserver())
	waitForLifecycle(t, events, journal.AgentWaiting)
	sampler.stop()

	// stop WAITS for the goroutine, so nothing can be in flight here.
	out := Outcome{}
	var runErr error
	emitter.finish(&out, &runErr)
	if runErr != nil {
		t.Fatalf("finish reported %v", runErr)
	}

	got := lifecycles(events())
	terminal := -1
	for i, l := range got {
		if l == journal.AgentCompleted || l == journal.AgentFailed {
			terminal = i
		}
	}
	if terminal < 0 {
		t.Fatalf("no terminal lifecycle event in %v", got)
	}
	if terminal != len(got)-1 {
		t.Fatalf("a liveness mark landed after the terminal event: %v", got)
	}
}

// TestActivitySampleReportsIdleAndMovement pins the sample's own semantics,
// since the lifecycle mapping is only as good as what it reads.
func TestActivitySampleReportsIdleAndMovement(t *testing.T) {
	start := time.Now()
	tracker := newActivityTracker(start)

	// Nothing written yet: idle equals elapsed, because "silent since start"
	// is the honest reading, not "idle for zero seconds".
	sample, writes := tracker.sample(start.Add(time.Minute), 0, 0)
	if sample.Idle != time.Minute || sample.Elapsed != time.Minute {
		t.Fatalf("silent session: elapsed=%s idle=%s, want both 1m", sample.Elapsed, sample.Idle)
	}
	if sample.Moved {
		t.Error("a session that wrote nothing reported movement")
	}

	tracker.observe(start.Add(90 * time.Second))
	sample, writes = tracker.sample(start.Add(2*time.Minute), writes, 512)
	if !sample.Moved {
		t.Error("output arrived since the previous sample but Moved is false")
	}
	if sample.Idle != 30*time.Second {
		t.Errorf("idle = %s, want 30s since the last write", sample.Idle)
	}
	if sample.Bytes != 512 {
		t.Errorf("bytes = %d, want 512", sample.Bytes)
	}

	// No further writes: the next sample must fall back to waiting, and idle
	// must keep growing. This is what makes a run of marks readable as
	// "silent since T".
	sample, _ = tracker.sample(start.Add(3*time.Minute), writes, 512)
	if sample.Moved {
		t.Error("no output arrived but Moved is true")
	}
	if sample.Idle != 90*time.Second {
		t.Errorf("idle = %s, want 90s and growing", sample.Idle)
	}
}

// TestObservedBytesCountsDroppedOutput: a session chatty enough to pass the
// transcript cap is emphatically not stalled. Counting only retained bytes
// would freeze the number at the cap and make the loudest possible session
// look like the quietest.
func TestObservedBytesCountsDroppedOutput(t *testing.T) {
	buf := newTranscriptBuffer(8)
	if _, err := buf.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if got := buf.observedBytes(); got != 16 {
		t.Fatalf("observedBytes = %d, want 16 (8 retained + 8 dropped)", got)
	}
}

// TestActivitySamplerStopsOnContextCancel: when the process is being killed,
// its terminal event already says so. A liveness mark racing that would only
// add noise to the one moment that is already legible.
func TestActivitySamplerStopsOnContextCancel(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	var samples int
	var mu sync.Mutex
	sampler := startActivitySampler(ctx, newActivityTracker(time.Now()), time.Hour, nil, func(ActivitySample) {
		mu.Lock()
		samples++
		mu.Unlock()
	})
	cancel()
	sampler.stop()
	mu.Lock()
	defer mu.Unlock()
	if samples != 0 {
		t.Errorf("sampler emitted %d marks after cancellation", samples)
	}
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Errorf("sampler goroutine outlived stop: %d before, %d after", before, after)
	}
}

// TestNilObserverStartsNoSampler covers the shape every non-agentic caller
// has, and the reason stop is safe to defer unconditionally.
func TestNilObserverStartsNoSampler(t *testing.T) {
	if s := startActivitySampler(context.Background(), newActivityTracker(time.Now()), time.Millisecond, nil, nil); s != nil {
		t.Fatalf("a nil observer produced a sampler: %v", s)
	}
	var nilSampler *activitySampler
	nilSampler.stop()

	var nilEmitter *adapterAgentEmitter
	if nilEmitter.activityObserver() != nil {
		t.Error("a nil emitter produced an observer")
	}
}

func waitForLifecycle(t *testing.T, events func() []journal.Event, want journal.AgentLifecycle) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range lifecycles(events()) {
			if l == want {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no %q lifecycle mark within the deadline; saw %v", want, lifecycles(events()))
}

// TestProcessRunnerReportsStallOfARealSubprocess is the end-to-end half: the
// unit tests above drive the sampler directly, which would still pass if the
// observer were never wired into ExecProcessRunner.Run at all — the exact
// shape of gap that let #4179 happen. This runs a real, deliberately silent
// subprocess and requires the marks to arrive.
func TestProcessRunnerReportsStallOfARealSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep(1) is not the shell builtin shape this asserts on Windows")
	}
	var mu sync.Mutex
	var samples []ActivitySample
	_, err := ExecProcessRunner{}.Run(context.Background(), ProcessRequest{
		// Silent for its whole life, like a session blocked on a module
		// download: no output at all, but very much alive.
		Command:          []string{"sleep", "1"},
		Env:              []string{},
		Timeout:          30 * time.Second,
		ActivityInterval: 20 * time.Millisecond,
		Activity: func(s ActivitySample) {
			mu.Lock()
			defer mu.Unlock()
			samples = append(samples, s)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(samples) == 0 {
		t.Fatal("a silent one-second subprocess produced no activity samples: the observer is not wired into Run (#4179)")
	}
	for i, s := range samples {
		if s.Moved {
			t.Errorf("sample %d of a silent subprocess reports movement: %+v", i, s)
		}
		if s.Idle < s.Elapsed {
			t.Errorf("sample %d: idle %s < elapsed %s, but nothing was ever written", i, s.Idle, s.Elapsed)
		}
	}
}

// TestProcessRunnerReportsMovementOfAChattySubprocess is its complement: the
// same wiring must report a producing session as producing, or every session
// would read as stalled and the signal would be worthless.
func TestProcessRunnerReportsMovementOfAChattySubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relies on a POSIX shell loop")
	}
	var mu sync.Mutex
	var moved bool
	_, err := ExecProcessRunner{}.Run(context.Background(), ProcessRequest{
		Command:          []string{"sh", "-c", "i=0; while [ $i -lt 40 ]; do echo working; sleep 0.02; i=$((i+1)); done"},
		Env:              []string{"PATH=/bin:/usr/bin"},
		Timeout:          30 * time.Second,
		ActivityInterval: 20 * time.Millisecond,
		Activity: func(s ActivitySample) {
			mu.Lock()
			defer mu.Unlock()
			if s.Moved {
				moved = true
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !moved {
		t.Fatal("a subprocess writing continuously never reported movement: waiting and working are still indistinguishable (#4179)")
	}
}
