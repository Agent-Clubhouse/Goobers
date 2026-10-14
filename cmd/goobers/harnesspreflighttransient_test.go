package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/localscheduler"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeHarnessClock is a settable clock for the harness probe backoff.
type fakeHarnessClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeHarnessClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeHarnessClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// withHarnessProbeThrottle installs a fresh probe backoff on a fake clock for
// the duration of the test, so no transient failure leaks between tests.
func withHarnessProbeThrottle(t *testing.T) *fakeHarnessClock {
	t.Helper()
	clock := &fakeHarnessClock{now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	previous := harnessProbeThrottle
	harnessProbeThrottle = newHarnessProbeBackoff(clock.Now)
	t.Cleanup(func() { harnessProbeThrottle = previous })
	return clock
}

// rateLimitedPreflight is what CopilotAdapter.Preflight now returns for a
// rate-limited sign-in probe (internal/harness covers that classification).
func rateLimitedPreflight(retryAfter time.Duration) error {
	return &harness.TransientPreflightError{
		Err:        errors.New(`harness: copilot-cli: sign-in check exited 1: Failed to fetch PAT user login (403): API rate limit exceeded`),
		RetryAfter: retryAfter,
	}
}

func TestHarnessProbeBackoffSchedule(t *testing.T) {
	clock := &fakeHarnessClock{now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	backoff := newHarnessProbeBackoff(clock.Now)
	transient := rateLimitedPreflight(0)

	// Without a Retry-After: 1m, doubling per consecutive failure, capped.
	var waits []time.Duration
	for range 7 {
		start := clock.Now()
		retryAt := backoff.record("copilot", transient)
		waits = append(waits, retryAt.Sub(start))
		if _, waiting := backoff.pending("copilot"); !waiting {
			t.Fatal("a just-recorded transient failure is not being waited out")
		}
		clock.Advance(retryAt.Sub(start) - time.Second)
		if _, waiting := backoff.pending("copilot"); !waiting {
			t.Fatal("the backoff elapsed early")
		}
		clock.Advance(time.Second)
		if _, waiting := backoff.pending("copilot"); waiting {
			t.Fatal("the backoff did not elapse at its retry time")
		}
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("backoff waits = %v, want %v", waits, want)
		}
	}

	// A provider Retry-After is honoured as given.
	start := clock.Now()
	if retryAt := backoff.record("copilot", rateLimitedPreflight(90*time.Second)); retryAt.Sub(start) != 90*time.Second {
		t.Fatalf("retry-after wait = %s, want 90s", retryAt.Sub(start))
	}

	// Another key is independent, and clear resets the schedule.
	if _, waiting := backoff.pending("claude-code"); waiting {
		t.Fatal("an unrelated harness group was backed off")
	}
	backoff.clear("copilot")
	start = clock.Now()
	if retryAt := backoff.record("copilot", transient); retryAt.Sub(start) != time.Minute {
		t.Fatalf("wait after clear = %s, want the initial 1m", retryAt.Sub(start))
	}
}

// countingHarnessAdapters installs adapters whose copilot preflight returns
// whatever copilotErr holds, counting each copilot preflight.
type countingHarnessAdapters struct {
	mu         sync.Mutex
	copilotErr error
	claudeErr  error
	probes     map[apiv1.Harness]int
}

func installCountingHarnessAdapters(t *testing.T, copilotErr error) *countingHarnessAdapters {
	t.Helper()
	adapters := &countingHarnessAdapters{copilotErr: copilotErr, probes: map[apiv1.Harness]int{}}
	withHarnessAdapter(t, func(h apiv1.Harness, _ harness.EnvironmentConfig, _ map[string][]string, _ func(context.Context) (string, error)) (harness.Adapter, error) {
		adapters.mu.Lock()
		defer adapters.mu.Unlock()
		adapters.probes[h]++
		err := adapters.claudeErr
		if h == apiv1.HarnessCopilot {
			err = adapters.copilotErr
		}
		return &harnesstest.FakeAdapter{AdapterName: string(h), Version: string(h) + " 1.0.0", PreflightErr: err}, nil
	})
	return adapters
}

func (a *countingHarnessAdapters) set(copilotErr, claudeErr error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.copilotErr, a.claudeErr = copilotErr, claudeErr
}

func (a *countingHarnessAdapters) count(h apiv1.Harness) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.probes[h]
}

func transientPreflightFixture() (map[string]apiv1.GooberSpec, []apiv1.Workflow) {
	goobers := map[string]apiv1.GooberSpec{
		"coder":    {Harness: apiv1.HarnessCopilot},
		"reviewer": {Harness: apiv1.HarnessClaudeCode},
	}
	workflows := []apiv1.Workflow{
		{ObjectMeta: metav1.ObjectMeta{Name: "copilot-only"}, Spec: apiv1.WorkflowSpec{Gaggle: "example", Tasks: []apiv1.Task{{Name: "write", Type: apiv1.TaskAgentic, Goober: "coder"}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "both"}, Spec: apiv1.WorkflowSpec{Gaggle: "example",
			Tasks: []apiv1.Task{{Name: "write", Type: apiv1.TaskAgentic, Goober: "coder"}},
			Gates: []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"}}},
		}},
	}
	return goobers, workflows
}

var (
	copilotOnly   = localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "copilot-only"}
	bothHarnesses = localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "both"}
)

// TestPreflightBacksOffATransientHarnessFailure is #6017's bound on the
// re-probe rate: a rate-limited sign-in probe is a transient refusal that is
// not probed again until its backoff elapses, however often the preflight
// runs (the worker re-runs it on every activity of a degraded kit).
func TestPreflightBacksOffATransientHarnessFailure(t *testing.T) {
	clock := withHarnessProbeThrottle(t)
	adapters := installCountingHarnessAdapters(t, rateLimitedPreflight(0))
	goobers, workflows := transientPreflightFixture()

	_, err := preflightAgenticHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil)
	var failures *harnessPreflightFailures
	if !errors.As(err, &failures) {
		t.Fatalf("error = %T %v, want scoped *harnessPreflightFailures", err, err)
	}
	refusal := failures.transientRefusal(copilotOnly)
	if refusal == nil || !refusal.retryAt.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("transient refusal = %+v, want a retry one backoff (1m) out", refusal)
	}
	if reason := failures.Refusals[copilotOnly]; !strings.Contains(reason, "API rate limit exceeded") || !strings.Contains(reason, "transient provider failure; next probe after 2026-10-14T12:01:00Z") {
		t.Fatalf("refusal = %q, want the provider cause and the next probe time", reason)
	}
	if failures.recheck(copilotOnly) == nil || failures.recheck(bothHarnesses) == nil {
		t.Fatal("a wholly transient refusal carries no recheck")
	}

	// Within the backoff, the preflight refuses again WITHOUT probing.
	for range 3 {
		clock.Advance(15 * time.Second)
		if _, err := preflightAgenticHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil); err == nil {
			t.Fatal("a backed-off harness was admitted without a probe")
		}
	}
	if got := adapters.count(apiv1.HarnessCopilot); got != 1 {
		t.Fatalf("copilot probed %d times within one backoff window, want 1", got)
	}
	if got := adapters.count(apiv1.HarnessClaudeCode); got != 4 {
		t.Fatalf("the healthy harness was probed %d times, want every preflight (4)", got)
	}

	// Once it elapses the probe runs again; a second transient failure
	// doubles the wait.
	clock.Advance(15 * time.Second)
	_, err = preflightAgenticHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil)
	if !errors.As(err, &failures) || adapters.count(apiv1.HarnessCopilot) != 2 {
		t.Fatalf("after the backoff: err=%v probes=%d, want a second probe", err, adapters.count(apiv1.HarnessCopilot))
	}
	if refusal := failures.transientRefusal(copilotOnly); refusal == nil || !refusal.retryAt.Equal(clock.Now().Add(2*time.Minute)) {
		t.Fatalf("second transient refusal = %+v, want a doubled 2m backoff", refusal)
	}

	// Recovery admits the harness and clears the backoff.
	adapters.set(nil, nil)
	clock.Advance(2 * time.Minute)
	if _, err := preflightAgenticHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil); err != nil {
		t.Fatalf("recovered harness still refused: %v", err)
	}
	adapters.set(rateLimitedPreflight(0), nil)
	_, err = preflightAgenticHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil)
	if !errors.As(err, &failures) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if refusal := failures.transientRefusal(copilotOnly); refusal == nil || !refusal.retryAt.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("refusal after recovery = %+v, want the backoff restarted at 1m", refusal)
	}
}

// TestPreflightHonoursProviderRetryAfter: the provider's Retry-After, when the
// probe reported one, is the wait.
func TestPreflightHonoursProviderRetryAfter(t *testing.T) {
	clock := withHarnessProbeThrottle(t)
	installCountingHarnessAdapters(t, rateLimitedPreflight(7*time.Minute))
	goobers, workflows := transientPreflightFixture()
	_, err := preflightAgenticHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil)
	var failures *harnessPreflightFailures
	if !errors.As(err, &failures) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if refusal := failures.transientRefusal(copilotOnly); refusal == nil || !refusal.retryAt.Equal(clock.Now().Add(7*time.Minute)) {
		t.Fatalf("refusal = %+v, want the provider's 7m Retry-After", refusal)
	}
}

// TestPreflightKeepsPermanentHarnessFailuresPermanent: only a transient
// provider failure is retryable. A signed-out harness keeps its permanent
// refusal and is not backed off, and a workflow refused for BOTH a transient
// and a permanent cause is permanently refused.
func TestPreflightKeepsPermanentHarnessFailuresPermanent(t *testing.T) {
	withHarnessProbeThrottle(t)
	adapters := installCountingHarnessAdapters(t, rateLimitedPreflight(0))
	adapters.set(rateLimitedPreflight(0), errors.New("claude auth status exited 1: not logged in"))
	goobers, workflows := transientPreflightFixture()

	_, err := preflightAgenticHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil)
	var failures *harnessPreflightFailures
	if !errors.As(err, &failures) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if failures.recheck(copilotOnly) == nil {
		t.Fatal("the copilot-only workflow's refusal is wholly transient and must carry a recheck")
	}
	if failures.transientRefusal(bothHarnesses) != nil || failures.recheck(bothHarnesses) != nil {
		t.Fatal("a workflow refused for a signed-out harness was treated as transient")
	}
	if reason := failures.Refusals[bothHarnesses]; !strings.Contains(reason, "not logged in") || !strings.Contains(reason, "API rate limit exceeded") {
		t.Fatalf("refusal = %q, want both causes named", reason)
	}
	// The permanent failure is re-probed on the next preflight, as before.
	if _, err := preflightAgenticHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil); err == nil {
		t.Fatal("expected the second preflight to refuse")
	}
	if got := adapters.count(apiv1.HarnessClaudeCode); got != 2 {
		t.Fatalf("permanent failure probed %d times, want 2 (no backoff)", got)
	}
}

// TestDaemonTransientHarnessRefusalRechecks is the daemon half of #6017: a
// workflow refused at startup because its harness's provider was rate limited
// is re-probed in the background once the backoff elapses, and admitted again
// when the provider recovers, without a restart or configuration change.
func TestDaemonTransientHarnessRefusalRechecks(t *testing.T) {
	clock := withHarnessProbeThrottle(t)
	adapters := installCountingHarnessAdapters(t, rateLimitedPreflight(0))
	previous := preflightHarnesses
	preflightHarnesses = preflightAgenticHarnesses
	t.Cleanup(func() { preflightHarnesses = previous })
	goobers, workflows := transientPreflightFixture()

	_, failures, err := preflightDaemonHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	recheck, ok := failures.recheck(copilotOnly).(*harnessWorkflowRecheck)
	if !ok || failures.reason(copilotOnly) == "" {
		t.Fatalf("daemon refusal = %q recheck=%T, want a transient refusal with a recheck", failures.reason(copilotOnly), failures.recheck(copilotOnly))
	}
	synchronousRecheck(recheck)

	// Before the backoff elapses: still refused, no probe.
	if refusal := recheck.HarnessRefusal(clock.Advance(30 * time.Second)); !strings.Contains(refusal, `stage "write"`) || !strings.Contains(refusal, "API rate limit exceeded") {
		t.Fatalf("refusal = %q, want the stage and the provider cause", refusal)
	}
	if got := adapters.count(apiv1.HarnessCopilot); got != 1 {
		t.Fatalf("recheck probed before its backoff elapsed: %d probes", got)
	}

	// Due and still rate limited: one re-probe, still refused.
	recheck.HarnessRefusal(clock.Advance(30 * time.Second))
	if got := adapters.count(apiv1.HarnessCopilot); got != 2 {
		t.Fatalf("due recheck probes = %d, want 2", got)
	}
	if refusal := recheck.HarnessRefusal(clock.Now()); refusal == "" || adapters.count(apiv1.HarnessCopilot) != 2 {
		t.Fatalf("still-limited recheck = %q (probes %d), want refused without another probe", refusal, adapters.count(apiv1.HarnessCopilot))
	}

	// Recovered: the next due probe clears the refusal.
	adapters.set(nil, nil)
	recheck.HarnessRefusal(clock.Advance(2 * time.Minute))
	if refusal := recheck.HarnessRefusal(clock.Now()); refusal != "" {
		t.Fatalf("recovered harness still refused: %q", refusal)
	}
	probes := adapters.count(apiv1.HarnessCopilot)
	recheck.HarnessRefusal(clock.Advance(time.Hour))
	if adapters.count(apiv1.HarnessCopilot) != probes {
		t.Fatal("a recovered recheck kept probing")
	}
}

// TestDaemonHarnessRecheckStopsOnPermanentFailure: a re-probe that finds a
// real sign-in problem makes the refusal permanent, as it would have been at
// startup, rather than re-probing every pass.
func TestDaemonHarnessRecheckStopsOnPermanentFailure(t *testing.T) {
	clock := withHarnessProbeThrottle(t)
	adapters := installCountingHarnessAdapters(t, rateLimitedPreflight(0))
	previous := preflightHarnesses
	preflightHarnesses = preflightAgenticHarnesses
	t.Cleanup(func() { preflightHarnesses = previous })
	goobers, workflows := transientPreflightFixture()

	_, failures, err := preflightDaemonHarnesses(goobers, workflows, harness.EnvironmentConfig{}, nil, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	recheck := failures.recheck(copilotOnly).(*harnessWorkflowRecheck)
	synchronousRecheck(recheck)
	adapters.set(&harness.AuthRequiredError{Err: errors.New("not signed in")}, nil)
	recheck.HarnessRefusal(clock.Advance(time.Minute))
	if refusal := recheck.HarnessRefusal(clock.Now()); !strings.Contains(refusal, "not signed in") {
		t.Fatalf("refusal = %q, want the sign-in failure", refusal)
	}
	probes := adapters.count(apiv1.HarnessCopilot)
	for range 3 {
		recheck.HarnessRefusal(clock.Advance(time.Hour))
	}
	if adapters.count(apiv1.HarnessCopilot) != probes {
		t.Fatal("a permanently failed recheck kept probing")
	}
}

// synchronousRecheck runs a recheck's re-probes on the caller's goroutine.
func synchronousRecheck(recheck *harnessWorkflowRecheck) {
	for _, use := range recheck.uses {
		use.group.start = func(probe func()) { probe() }
	}
}

// TestWorkerTransientHarnessRefusalCarriesRetryTime: the worker's refusal of
// a transiently failed harness is an infrastructure failure that names when
// the harness may next be probed, so the engine's retry waits for it; a
// permanent refusal stays a plain infrastructure failure (#5949).
func TestWorkerTransientHarnessRefusalCarriesRetryTime(t *testing.T) {
	retryAt := time.Date(2026, 10, 14, 12, 5, 0, 0, time.UTC)
	seams := &gaggleSeams{
		harnessRefusals: map[localscheduler.WorkflowIdentity]string{
			copilotOnly:   `stage "write" requires harness "copilot": API rate limit exceeded`,
			bothHarnesses: `stage "review" requires harness "claude-code": not logged in`,
		},
		harnessRetryAt: map[localscheduler.WorkflowIdentity]time.Time{copilotOnly: retryAt},
	}
	err := seams.refuseUnavailableHarness(apiv1.InvocationEnvelope{Gaggle: "example", WorkflowID: "copilot-only"})
	if got, ok := invoke.InfrastructureRetryAt(err); !invoke.IsInfrastructureFailure(err) || !ok || !got.Equal(retryAt) {
		t.Fatalf("transient refusal = %v (retryAt %s, %t), want an infrastructure failure retrying at %s", err, got, ok, retryAt)
	}
	err = seams.refuseUnavailableHarness(apiv1.InvocationEnvelope{Gaggle: "example", WorkflowID: "both"})
	if _, ok := invoke.InfrastructureRetryAt(err); !invoke.IsInfrastructureFailure(err) || ok {
		t.Fatalf("permanent refusal = %v, want a plain infrastructure failure", err)
	}
}
