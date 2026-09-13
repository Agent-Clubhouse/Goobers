package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

// TestDaemonProbeStateLivenessGraceBeforeFirstTick locks the pre-first-tick
// grace window: a long legitimate crash-resume must not read as a wedged
// main loop, regardless of how stale (or absent) the heartbeat is.
func TestDaemonProbeStateLivenessGraceBeforeFirstTick(t *testing.T) {
	var schedulerTicked atomic.Bool
	var lastTickAtNanos atomic.Int64
	now := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	state := &daemonProbeState{
		schedulerTicked: &schedulerTicked,
		lastTickAtNanos: &lastTickAtNanos,
		livenessTimeout: time.Minute,
		now:             func() time.Time { return now },
	}
	if !state.liveness() {
		t.Fatal("liveness() before the scheduler's first tick must default healthy (startup grace)")
	}
}

func TestTriggerSweepProgressRequiresSuccessfulCompletion(t *testing.T) {
	var heartbeat atomic.Int64
	now := time.Now()
	refused := errors.New("trigger storage unavailable")
	if err := recordTriggerSweepProgress(&heartbeat, refused, now); !errors.Is(err, refused) || heartbeat.Load() != 0 {
		t.Fatalf("failed sweep advanced readiness: %v %d", err, heartbeat.Load())
	}
	if err := recordTriggerSweepProgress(&heartbeat, nil, now); err != nil || heartbeat.Load() != now.UnixNano() {
		t.Fatalf("successful sweep did not advance readiness: %v %d", err, heartbeat.Load())
	}
	if err := recordTriggerSweepProgress(&heartbeat, refused, now.Add(time.Minute)); !errors.Is(err, refused) || heartbeat.Load() != now.UnixNano() {
		t.Fatal("subsequent failed sweep hid stale progress")
	}
}

func TestDaemonProbeHTTPDistinguishesListeningFromTriggerReadiness(t *testing.T) {
	var listening, planeReady, ready, configLoaded, stateOpen, resumeComplete, sweepsStarted atomic.Bool
	var lastTick, lastSweep atomic.Int64
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	started := now
	state := &daemonProbeState{
		apiListening: &listening, planeReady: &planeReady, ready: &ready, configLoaded: &configLoaded,
		stateOpen: &stateOpen, resumeComplete: &resumeComplete, sweepsStarted: &sweepsStarted,
		lastTickAtNanos: &lastTick, lastTriggerSweepAtNanos: &lastSweep,
		livenessTimeout: time.Minute, now: func() time.Time { return now },
	}
	handler := httpapi.WrapWithProbes(http.NotFoundHandler(), nil, state.readiness)
	probe := func(wantStatus int, wantScheduler, wantSweep bool) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpapi.ReadinessPath, nil))
		var result httpapi.ReadinessStatus
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if response.Code != wantStatus || !result.Checks["apiListening"] || result.Checks["schedulerReady"] != wantScheduler || result.Checks["triggerSweepReady"] != wantSweep {
			t.Fatalf("readiness status=%d body=%s", response.Code, response.Body.String())
		}
	}
	listening.Store(true)
	for _, delay := range []time.Duration{35 * time.Second, 60 * time.Second} {
		now = started.Add(delay)
		// The bare listener answers (apiListening) for a minute before the
		// full handler — and so plane-ready — is even wired in.
		probe(http.StatusServiceUnavailable, false, false)
	}
	configLoaded.Store(true)
	stateOpen.Store(true)
	// #4252: plane-ready (the full handler, credential/blob/journal/surrender
	// routes included, swapped into the SwitchHandler) flips here — well
	// before resumeComplete/sweepsStarted/ready — and the HTTP status must
	// flip to 200 with it, independent of scheduler-readiness below.
	planeReady.Store(true)
	probe(http.StatusOK, false, false)
	resumeComplete.Store(true)
	sweepsStarted.Store(true)
	ready.Store(true)
	probe(http.StatusOK, false, false) // no observed loop progress yet
	lastTick.Store(now.UnixNano())
	lastSweep.Store(now.UnixNano())
	probe(http.StatusOK, true, true)
	now = now.Add(2 * time.Minute)
	lastTick.Store(now.UnixNano())
	probe(http.StatusOK, true, false) // scheduler live, trigger sweep stalled
	lastSweep.Store(now.UnixNano())
	probe(http.StatusOK, true, true)
	ready.Store(false)
	// #4252: scheduler-ready dropping (schedulerReady/triggerSweepReady
	// checks go false) no longer takes the HTTP status back to 503 — Ready is
	// plane-ready, and the plane has not gone anywhere.
	probe(http.StatusOK, false, false)
}

// TestDaemonProbeStateLivenessReflectsHeartbeatStaleness is the direct,
// no-daemon-required test of #3806's liveness closure the reviewers asked
// for: it must go unhealthy once the heartbeat exceeds livenessTimeout, and
// this is exactly the behavior a hardcoded `return true` (the mutation
// the PR's own ablation evidence used) would silently drop.
func TestDaemonProbeStateLivenessReflectsHeartbeatStaleness(t *testing.T) {
	var schedulerTicked atomic.Bool
	var lastTickAtNanos atomic.Int64
	now := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	state := &daemonProbeState{
		schedulerTicked: &schedulerTicked,
		lastTickAtNanos: &lastTickAtNanos,
		livenessTimeout: time.Minute,
		now:             func() time.Time { return now },
	}

	schedulerTicked.Store(true)
	lastTickAtNanos.Store(now.UnixNano())
	if !state.liveness() {
		t.Fatal("liveness() with a fresh heartbeat must be healthy")
	}

	now = now.Add(2 * time.Minute) // exceeds the 1-minute livenessTimeout
	if state.liveness() {
		t.Fatal("liveness() with a heartbeat stale beyond livenessTimeout must be unhealthy — a wedged main loop must be observable")
	}

	now = now.Add(-90 * time.Second) // back within budget (30s stale)
	if !state.liveness() {
		t.Fatal("liveness() must recover once a fresh-enough heartbeat is observed again")
	}
}

// TestDaemonProbeStateReadinessReflectsReadyGate is the direct test of
// #3806's readiness closure: Ready must reflect the `ready` gate, not
// default true. A hardcoded `Ready: true` (this PR's own literal
// regression, per issue #3806) would pass every existing test that only
// exercises the post-startup happy path; this one specifically starts from
// ready=false.
func TestDaemonProbeStateReadinessReflectsReadyGate(t *testing.T) {
	var ready, planeReady, configLoaded, stateOpen, resumeComplete, sweepsStarted atomic.Bool
	started := time.Date(2026, time.September, 12, 19, 30, 0, 0, time.UTC)
	tracker := &startupPhaseTracker{}
	tracker.set("worktree-reap-crash-orphan", "efunhouse")
	state := &daemonProbeState{
		ready:          &ready,
		planeReady:     &planeReady,
		configLoaded:   &configLoaded,
		stateOpen:      &stateOpen,
		resumeComplete: &resumeComplete,
		sweepsStarted:  &sweepsStarted,
		startup:        tracker,
	}
	tracker.mu.Lock()
	tracker.started = started
	tracker.mu.Unlock()

	got := state.readiness()
	if got.Ready || got.SchedulerReady {
		t.Fatal("readiness() must be false before its gates flip, not default true")
	}
	for name, value := range got.Checks {
		if value {
			t.Fatalf("check %q = true before anything ran, want false", name)
		}
	}
	if got.Startup == nil || got.Startup.Phase != "worktree-reap-crash-orphan" ||
		!got.Startup.Since.Equal(started) {
		t.Fatalf("startup = %+v, want current worktree reap phase", got.Startup)
	}
	handler := httpapi.WrapWithProbes(http.NotFoundHandler(), nil, state.readiness)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpapi.ReadinessPath, nil))
	if strings.Contains(response.Body.String(), "efunhouse") {
		t.Fatalf("public readiness disclosed startup target: %s", response.Body.String())
	}

	ready.Store(true)
	planeReady.Store(true)
	configLoaded.Store(true)
	stateOpen.Store(true)
	resumeComplete.Store(true)
	sweepsStarted.Store(true)

	got = state.readiness()
	if !got.Ready {
		t.Fatal("readiness() must flip true once the plane-ready gate is set")
	}
	if !got.SchedulerReady {
		t.Fatal("readiness() SchedulerReady must flip true once the scheduler-ready gate is set")
	}
	if got.Startup != nil {
		t.Fatalf("ready daemon reported startup phase: %+v", got.Startup)
	}
	for _, name := range []string{"configLoaded", "stateOpen", "resumeComplete", "sweepsStarted"} {
		if !got.Checks[name] {
			t.Fatalf("check %q = false once every subsystem flipped true, checks = %+v", name, got.Checks)
		}
	}
}

// TestDaemonProbeStateReadinessPlaneReadyIndependentOfSchedulerReady is
// #4252's core regression: Ready (plane-ready, the gate the Kubernetes
// Service/readinessProbe now consumes) must be able to flip true while
// SchedulerReady (resumeComplete/sweepsStarted, "safe to admit new dispatch")
// is still false — that gap is exactly the long crash-resume window a
// running stage pod must be able to reach its planes during, without the
// daemon yet admitting brand-new scheduling.
func TestDaemonProbeStateReadinessPlaneReadyIndependentOfSchedulerReady(t *testing.T) {
	var ready, planeReady, configLoaded, stateOpen, resumeComplete, sweepsStarted atomic.Bool
	state := &daemonProbeState{
		ready:          &ready,
		planeReady:     &planeReady,
		configLoaded:   &configLoaded,
		stateOpen:      &stateOpen,
		resumeComplete: &resumeComplete,
		sweepsStarted:  &sweepsStarted,
	}

	configLoaded.Store(true)
	stateOpen.Store(true)
	planeReady.Store(true)
	// resumeComplete, sweepsStarted, and the overall scheduler-ready gate
	// remain false: crash-resume is still in progress.

	got := state.readiness()
	if !got.Ready {
		t.Fatal("Ready (plane-ready) must be true once the listener/planes are open, even mid crash-resume")
	}
	if got.SchedulerReady {
		t.Fatal("SchedulerReady must stay false while crash-resume has not completed")
	}
	if got.Checks["resumeComplete"] {
		t.Fatal("resumeComplete check must still read false")
	}
}

// TestDaemonProbeStateReadinessPlaneReadyNilDefaultsFalse locks that an
// unwired planeReady pointer (e.g. an older construction site that has not
// been updated) fails closed rather than reporting ready by omission.
func TestDaemonProbeStateReadinessPlaneReadyNilDefaultsFalse(t *testing.T) {
	var ready, configLoaded, stateOpen, resumeComplete, sweepsStarted atomic.Bool
	state := &daemonProbeState{
		ready:          &ready,
		configLoaded:   &configLoaded,
		stateOpen:      &stateOpen,
		resumeComplete: &resumeComplete,
		sweepsStarted:  &sweepsStarted,
	}
	if got := state.readiness(); got.Ready {
		t.Fatal("readiness() with a nil planeReady pointer must fail closed (Ready = false), not panic or default true")
	}
}
