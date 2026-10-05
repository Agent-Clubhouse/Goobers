package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
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
	tracker.configureBudget(2 * time.Minute)
	tracker.setWorktreeAccumulation(4)
	tracker.setRecoveryAccumulation(6)
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
	if got.Startup.WorktreeCount != 4 || got.Startup.RecoveryRunCount != 6 ||
		got.Startup.AccumulationCount != 10 ||
		got.Startup.BudgetSeconds != (2*time.Minute+10*startupBudgetPerCandidate).Seconds() ||
		got.Startup.BudgetState != "within-budget" {
		t.Fatalf("startup budget = %+v, want measured accumulation and derived budget", got.Startup)
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

// TestDaemonProbeHTTPNamesCrashResumeAsSchedulingBlocker is #5199's second
// ask: "a daemon that cannot schedule is not ready in any operationally
// useful sense; at minimum the distinction needs to be visible". /readyz's
// 200 reports plane-readiness deliberately (#4252), so the body has to say
// both that the scheduler is NOT ready and which phase is holding it —
// otherwise a 51-minute scheduling outage looks identical to a healthy pod.
func TestDaemonProbeHTTPNamesCrashResumeAsSchedulingBlocker(t *testing.T) {
	var listening, planeReady, ready, configLoaded, stateOpen, resumeComplete, sweepsStarted atomic.Bool
	var lastTick, lastSweep atomic.Int64
	started := time.Date(2026, 9, 16, 6, 10, 7, 0, time.UTC)
	tracker := &startupPhaseTracker{}
	tracker.set("crash-resume", "candidates=1481")
	tracker.observeRecoveryProgress(resumeOutcome{
		Total: 1481, Examined: 1481, Resumed: []string{"run-resumed"},
		Blocking: &resumeBlockingCandidate{
			RunID: "secret-run", Gaggle: "secret-gaggle", Workflow: "secret-workflow",
			Disposition: "resolving-generation", Operation: "resolve execution generation",
			StartedAt: started.Add(-time.Minute), LastProgressAt: started.Add(-10 * time.Second),
		},
	})
	state := &daemonProbeState{
		apiListening: &listening, planeReady: &planeReady, ready: &ready, configLoaded: &configLoaded,
		stateOpen: &stateOpen, resumeComplete: &resumeComplete, sweepsStarted: &sweepsStarted,
		lastTickAtNanos: &lastTick, lastTriggerSweepAtNanos: &lastSweep, startup: tracker,
		livenessTimeout: time.Minute, now: func() time.Time { return started.Add(35 * time.Minute) },
	}
	listening.Store(true)
	planeReady.Store(true)
	configLoaded.Store(true)
	stateOpen.Store(true)

	handler := httpapi.WrapWithProbes(http.NotFoundHandler(), nil, state.readiness)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, httpapi.ReadinessPath, nil))

	var status httpapi.ReadinessStatus
	if err := json.NewDecoder(recorder.Body).Decode(&status); err != nil {
		t.Fatalf("decode readiness body: %v", err)
	}
	if !status.Ready {
		t.Fatal("plane-ready must still report ready: the pod serves stage traffic during crash-resume")
	}
	if status.SchedulerReady || status.Checks["resumeComplete"] {
		t.Fatalf("readiness = %+v, want scheduling reported as not ready while crash-resume runs", status)
	}
	if status.Startup == nil || status.Startup.Phase != "crash-resume" {
		t.Fatalf("startup = %+v, want the blocking phase named", status.Startup)
	}
	if status.Startup.Target != "" {
		t.Fatalf("unauthenticated readiness leaked startup target %q", status.Startup.Target)
	}
	if status.Startup.BlockingCandidate == nil {
		t.Fatal("startup blocking candidate missing")
	}
	if status.Startup.BlockingCandidate.RunID != "" || status.Startup.BlockingCandidate.Gaggle != "" || status.Startup.BlockingCandidate.Workflow != "" {
		t.Fatalf("unauthenticated readiness leaked candidate identity: %+v", status.Startup.BlockingCandidate)
	}
	if status.Startup.BlockingCandidate.Operation != "resolve execution generation" ||
		status.Startup.BlockingCandidate.Progress.Examined != 1481 {
		t.Fatalf("startup blocking candidate = %+v, want operation and progress", status.Startup.BlockingCandidate)
	}
}

func TestInstanceReadinessIncludesCrashResumeCandidateIdentity(t *testing.T) {
	started := time.Now().Add(-2 * time.Minute)
	tracker := &startupPhaseTracker{}
	tracker.set("crash-resume", "candidates=2")
	tracker.observeRecoveryProgress(resumeOutcome{
		Total: 2, Examined: 2, Reattached: []string{"engine-run"},
		Blocking: &resumeBlockingCandidate{
			RunID: "run-blocked", Gaggle: "goobers", Workflow: "implement",
			Disposition: "dispatching", Operation: "journal resume annotation",
			StartedAt: started, LastProgressAt: started.Add(time.Minute),
		},
	})
	service := &daemonInstanceReadinessService{
		instanceRoot: t.TempDir(),
		tracker:      tracker,
		ready:        func() bool { return false },
	}

	status, err := service.InstanceReadiness(context.Background())
	if err != nil {
		t.Fatalf("InstanceReadiness: %v", err)
	}
	candidate := status.Recovery.BlockingCandidate
	if candidate == nil {
		t.Fatal("blocking candidate missing")
	}
	if candidate.RunID != "run-blocked" || candidate.Gaggle != "goobers" || candidate.Workflow != "implement" {
		t.Fatalf("candidate identity = %+v", candidate)
	}
	if candidate.Operation != "journal resume annotation" || candidate.Progress.Examined != 2 || candidate.Progress.Reattached != 1 {
		t.Fatalf("candidate progress = %+v", candidate)
	}
}

func TestCrashResumeBlockingCandidateSurfacesDuringLiveFinalCandidateBlock(t *testing.T) {
	const runID = "blocked-final-run"
	fixture := startBlockedFinalCandidateResume(t, runID)

	apiReadiness := fixture.instanceReadiness(t)
	assertBlockingCandidateReadiness(t, apiReadiness, runID)
	assertBlockingCandidatePublicProbe(t, fixture.publicReadiness(t), runID)
	assertBlockingCandidateCLI(t, fixture.root, runID)
	assertBlockingCandidatePortalPayload(t, fixture.tracker, runID)

	fixture.unblock(t)
	recovered := fixture.instanceReadiness(t)
	if !recovered.Ready || recovered.Recovery.Phase != "" || recovered.Recovery.BlockingCandidate != nil {
		t.Fatalf("readiness after unblock = %+v, want ready with no startup blocker", recovered)
	}
	if public := fixture.publicReadiness(t); !public.SchedulerReady || public.Startup != nil {
		t.Fatalf("public readiness after unblock = %+v, want scheduler-ready with no startup payload", public)
	}
	code, stdout, stderr := runArgs(t, "status", "--daemon", fixture.root)
	if code != 0 || stderr != "" {
		t.Fatalf("status --daemon after unblock: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if strings.Contains(stdout, "Crash recovery:") || strings.Contains(stdout, runID) {
		t.Fatalf("status --daemon after unblock kept stale recovery data: %q", stdout)
	}
}

type blockedFinalCandidateResumeFixture struct {
	root        string
	tracker     *startupPhaseTracker
	readiness   *daemonInstanceReadinessService
	ready       *atomic.Bool
	resumeDone  <-chan blockedResumeResult
	unblockOnce sync.Once
	unblockCh   chan struct{}
}

type blockedResumeResult struct {
	outcome resumeOutcome
	err     error
}

func startBlockedFinalCandidateResume(t *testing.T, runID string) *blockedFinalCandidateResumeFixture {
	t.Helper()
	const identity = "0123456789abcdef0123456789abcdef"
	t.Setenv("GOOBERS_API_TOKEN", "status-token")

	var readiness *daemonInstanceReadinessService
	root, _ := interventionCLIFixture(t, func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case apicontract.InstanceReadinessPath:
			status, err := readiness.InstanceReadiness(request.Context())
			if err != nil {
				t.Errorf("InstanceReadiness: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := json.NewEncoder(w).Encode(status); err != nil {
				t.Errorf("encode readiness: %v", err)
			}
		case apicontract.InstancePath:
			if err := json.NewEncoder(w).Encode(readservice.Instance{
				RootIdentity: &readservice.RootIdentity{ID: identity},
			}); err != nil {
				t.Errorf("encode instance: %v", err)
			}
		default:
			t.Errorf("request = %s %s, want readiness or instance", request.Method, request.URL.Path)
			http.NotFound(w, request)
		}
	})
	writeFileContent(t, filepath.Join(root, instance.RootIdentityFileName), identity+"\n")
	layout := instance.NewLayout(root)
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	skippedDir := filepath.Join(layout.RunsDir(), "aaa-not-a-run")
	if err := os.MkdirAll(skippedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: runID, Gaggle: "example", Workflow: "default-implement", WorkflowVersion: 1,
		ConfigGeneration: "blocked-generation", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(layout.RunsDir(), runID)

	tracker := &startupPhaseTracker{}
	tracker.set("crash-resume", "candidates=2")
	tracker.setRecoveryAccumulation(2)
	ready := &atomic.Bool{}
	readiness = &daemonInstanceReadinessService{
		instanceRoot: root,
		tracker:      tracker,
		ready:        ready.Load,
	}

	blocked := make(chan struct{})
	resolverEntered := make(chan struct{})
	unblock := make(chan struct{})
	var enteredOnce sync.Once
	registry := newDaemonRunnerRegistry()
	registry.setGenerationResolver(func(ctx context.Context, _ journal.RunIdentity) (executionGenerationRuntime, error) {
		enteredOnce.Do(func() { close(resolverEntered) })
		select {
		case <-unblock:
			return executionGenerationRuntime{}, nil
		case <-ctx.Done():
			return executionGenerationRuntime{}, ctx.Err()
		}
	})
	progress := func(outcome resumeOutcome) {
		tracker.observeRecoveryProgress(outcome)
		if outcome.Total == 2 && outcome.Examined == 2 && outcome.Blocking != nil &&
			outcome.Blocking.Operation == "resolve execution generation" {
			select {
			case <-blocked:
			default:
				close(blocked)
			}
		}
	}
	done := make(chan blockedResumeResult, 1)
	var resumeWG sync.WaitGroup
	go func() {
		outcome, err := resumeInterruptedRunsWithRunners(
			context.Background(), layout, nil, nil, registry, nil, nil, nil, nil, nil, nil, nil, nil,
			func(string, string) {}, &resumeWG, progress, []string{skippedDir, runDir},
		)
		resumeWG.Wait()
		done <- blockedResumeResult{outcome: outcome, err: err}
	}()

	waitForSignal(t, blocked, "blocking candidate progress")
	waitForSignal(t, resolverEntered, "generation resolver block")
	return &blockedFinalCandidateResumeFixture{
		root: root, tracker: tracker, readiness: readiness, ready: ready,
		resumeDone: done, unblockCh: unblock,
	}
}

func (f *blockedFinalCandidateResumeFixture) instanceReadiness(t *testing.T) httpapi.InstanceReadiness {
	t.Helper()
	status, err := f.readiness.InstanceReadiness(context.Background())
	if err != nil {
		t.Fatalf("InstanceReadiness: %v", err)
	}
	return status
}

func (f *blockedFinalCandidateResumeFixture) publicReadiness(t *testing.T) httpapi.ReadinessStatus {
	t.Helper()
	var listening, planeReady, configLoaded, stateOpen, sweepsStarted atomic.Bool
	listening.Store(true)
	planeReady.Store(true)
	configLoaded.Store(true)
	stateOpen.Store(true)
	state := &daemonProbeState{
		apiListening: &listening, planeReady: &planeReady, ready: f.ready, configLoaded: &configLoaded,
		stateOpen: &stateOpen, resumeComplete: f.ready, sweepsStarted: &sweepsStarted, startup: f.tracker,
		livenessTimeout: time.Minute, now: time.Now,
	}
	handler := httpapi.WrapWithProbes(http.NotFoundHandler(), nil, state.readiness)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, httpapi.ReadinessPath, nil))
	var status httpapi.ReadinessStatus
	if err := json.NewDecoder(recorder.Body).Decode(&status); err != nil {
		t.Fatalf("decode public readiness: %v", err)
	}
	return status
}

func (f *blockedFinalCandidateResumeFixture) unblock(t *testing.T) {
	t.Helper()
	f.unblockOnce.Do(func() { close(f.unblockCh) })
	result := waitForResumeResult(t, f.resumeDone)
	if result.err != nil {
		t.Fatalf("resumeInterruptedRunsWithRunners: %v", result.err)
	}
	if len(result.outcome.Warned) != 1 || result.outcome.Warned[0] != "blocked-final-run" || result.outcome.Blocking != nil {
		t.Fatalf("resume outcome after unblock = %+v, want final candidate skipped and blocker cleared", result.outcome)
	}
	f.tracker.clear("crash-resume")
	f.ready.Store(true)
}

func assertBlockingCandidateReadiness(t *testing.T, status httpapi.InstanceReadiness, runID string) {
	t.Helper()
	candidate := status.Recovery.BlockingCandidate
	if status.Ready || status.Recovery.Phase != "crash-resume" || candidate == nil {
		t.Fatalf("readiness = %+v, want crash-resume blocker", status)
	}
	if candidate.RunID != runID || candidate.Gaggle != "example" || candidate.Workflow != "default-implement" {
		t.Fatalf("candidate identity = %+v", candidate)
	}
	assertBlockingCandidateProgress(t, candidate.Progress, 2, 2, 0)
	if candidate.Disposition != "resolving-generation" || candidate.Operation != "resolve execution generation" {
		t.Fatalf("candidate state = %+v", candidate)
	}
}

func assertBlockingCandidatePublicProbe(t *testing.T, status httpapi.ReadinessStatus, runID string) {
	t.Helper()
	candidate := status.Startup.BlockingCandidate
	if !status.Ready || status.SchedulerReady || status.Startup == nil || candidate == nil {
		t.Fatalf("public readiness = %+v, want plane-ready scheduler-blocked startup", status)
	}
	if candidate.RunID != "" || candidate.Gaggle != "" || candidate.Workflow != "" {
		t.Fatalf("public readiness leaked identity for %q: %+v", runID, candidate)
	}
	assertBlockingCandidateProgress(t, candidate.Progress, 2, 2, 0)
	if candidate.Operation != "resolve execution generation" {
		t.Fatalf("public readiness candidate = %+v, want sanitized operation", candidate)
	}
}

func assertBlockingCandidateCLI(t *testing.T, root, runID string) {
	t.Helper()
	code, stdout, stderr := runArgs(t, "status", "--daemon", root)
	if code != 0 || stderr != "" {
		t.Fatalf("status --daemon: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	for _, want := range []string{
		"Startup recovery: phase=crash-resume",
		"Crash recovery: examined=2/2",
		"blocking run=" + runID,
		"workflow=example/default-implement",
		`operation="resolve execution generation"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("status --daemon output = %q, missing %q", stdout, want)
		}
	}
}

func assertBlockingCandidatePortalPayload(t *testing.T, tracker *startupPhaseTracker, runID string) {
	t.Helper()
	payload := readservice.Health{
		APIVersion: readservice.APIVersion, SchemaVersion: readservice.SchemaVersion,
		Ready: false, Healthy: true, Startup: readserviceStartupStatus(tracker, false),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"startup"`,
		`"blockingCandidate"`,
		`"runId":"` + runID + `"`,
		`"gaggle":"example"`,
		`"workflow":"default-implement"`,
		`"operation":"resolve execution generation"`,
		`"examined":2`,
		`"total":2`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("portal health payload = %s, missing %s", body, want)
		}
	}
}

func assertBlockingCandidateProgress(t *testing.T, progress httpapi.RecoveryProgress, examined, total, skipped int) {
	t.Helper()
	if progress.Examined != examined || progress.Total != total || progress.Skipped != skipped || progress.Resumed != 0 || progress.Reattached != 0 || progress.Terminal != 0 {
		t.Fatalf("progress = %+v, want examined=%d total=%d skipped=%d with no resumed candidates", progress, examined, total, skipped)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitForResumeResult(t *testing.T, done <-chan blockedResumeResult) blockedResumeResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for resume to unblock")
		return blockedResumeResult{}
	}
}
