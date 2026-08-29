package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func TestSelectRunnerLinuxPreferring(t *testing.T) {
	eligible := []RunnerSpec{
		{Name: "win-large", OS: "windows"},
		{Name: "tiny-linux", OS: "linux"},
	}
	runner, err := SelectRunner(testAttempt(), eligible)
	if err != nil {
		t.Fatalf("SelectRunner: %v", err)
	}
	if runner.Name != "tiny-linux" {
		t.Fatalf("picked %q, want the Linux runner (Linux-preferring placement, dsl-3.0.md D2)", runner.Name)
	}

	// No Linux in the set: first eligible wins (inventory order).
	winOnly := []RunnerSpec{{Name: "win-a", OS: "windows"}, {Name: "win-b", OS: "windows"}}
	runner, err = SelectRunner(testAttempt(), winOnly)
	if err != nil {
		t.Fatalf("SelectRunner: %v", err)
	}
	if runner.Name != "win-a" {
		t.Fatalf("picked %q, want first-in-inventory-order", runner.Name)
	}
}

func TestSelectRunnerEmptySetRefuses(t *testing.T) {
	_, err := SelectRunner(testAttempt(), nil)
	var selection *SelectionError
	if !errors.As(err, &selection) {
		t.Fatalf("got %v, want SelectionError", err)
	}
}

// Architecture §11.7 fact 1: a ledger-touching stage NEVER places on Windows;
// when the exclusion empties the set the refusal NAMES the structural fact
// (journaled/diagnosable), instead of a generic no-capacity shape.
func TestSelectRunnerLedgerTouchingNeverWindows(t *testing.T) {
	attempt := testAttempt()
	attempt.LedgerTouching = true

	// Windows-only eligible set: refused, cause named.
	_, err := SelectRunner(attempt, []RunnerSpec{{Name: "win-large", OS: "windows"}})
	var selection *SelectionError
	if !errors.As(err, &selection) {
		t.Fatalf("got %v, want SelectionError", err)
	}
	for _, needle := range []string{"ledger", "Windows", "win-large"} {
		if !strings.Contains(selection.Diagnostic, needle) {
			t.Errorf("diagnostic %q does not name %q", selection.Diagnostic, needle)
		}
	}

	// Mixed set: the Windows runner is excluded, the Linux one is taken.
	runner, err := SelectRunner(attempt, []RunnerSpec{
		{Name: "win-large", OS: "windows"},
		{Name: "tiny-linux", OS: "linux"},
	})
	if err != nil {
		t.Fatalf("SelectRunner: %v", err)
	}
	if runner.Name != "tiny-linux" {
		t.Fatalf("picked %q, want the non-Windows runner", runner.Name)
	}
}

// blockedProber never has capacity.
type blockedProber struct{ calls int }

func (p *blockedProber) Capacity(context.Context, RunnerSpec) (bool, error) {
	p.calls++
	return false, nil
}

// fakeClock drives the wait loop deterministically: every sleep advances the
// clock by the requested duration.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}

func newTestDispatcher(t *testing.T, cfg Config, pods PodAPI, capacity CapacityProber) (*Dispatcher, *fakeClock) {
	t.Helper()
	d, err := New(cfg, pods, nil, confirmGate{confirmed: true}, capacity)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	d.now = clock.Now
	d.sleep = clock.Sleep
	return d, clock
}

// §11.7 fact 2 / D12: a Windows dispatch that waits past the LINUX default
// bound produces a CAUSE-NAMING diagnostic — scale-from-zero node
// provisioning and image pulls are named — not a generic timeout; and the
// Windows bound is HIGHER than the Linux default.
func TestCapacityWaitWindowsCauseNamingDiagnostic(t *testing.T) {
	if DefaultWindowsScheduleToStart <= DefaultLinuxScheduleToStart {
		t.Fatal("the Windows schedule-to-start bound must exceed the Linux default (D12)")
	}
	prober := &blockedProber{}
	d, _ := newTestDispatcher(t, testConfig(), &fakePodAPI{}, prober)

	err := d.waitForCapacity(context.Background(), windowsRunner())
	var timeout *CapacityTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("got %v, want CapacityTimeoutError", err)
	}
	if timeout.Bound != DefaultWindowsScheduleToStart {
		t.Fatalf("Windows wait bound = %s, want %s", timeout.Bound, DefaultWindowsScheduleToStart)
	}
	message := err.Error()
	for _, needle := range []string{"scale-from-zero", "image pulls", "Windows", "Linux default bound"} {
		if !strings.Contains(message, needle) {
			t.Errorf("Windows capacity diagnostic %q does not name %q — a generic timeout is exactly what D12 forbids", message, needle)
		}
	}

	// The Linux diagnostic is bounded and named, without the Windows causes.
	err = d.waitForCapacity(context.Background(), linuxRunner())
	if !errors.As(err, &timeout) {
		t.Fatalf("got %v, want CapacityTimeoutError", err)
	}
	if timeout.Bound != DefaultLinuxScheduleToStart {
		t.Fatalf("Linux wait bound = %s, want %s", timeout.Bound, DefaultLinuxScheduleToStart)
	}
	if strings.Contains(err.Error(), "scale-from-zero node provisioning") {
		t.Error("Linux diagnostic borrows the Windows cause narrative")
	}
}

// A nil prober waits for nothing: Kubernetes queues the pod under its own
// activeDeadlineSeconds.
func TestCapacityWaitSkippedWithoutProber(t *testing.T) {
	d, _ := newTestDispatcher(t, testConfig(), &fakePodAPI{}, nil)
	if err := d.waitForCapacity(context.Background(), linuxRunner()); err != nil {
		t.Fatalf("nil prober should skip the wait, got %v", err)
	}
}

// Capacity arriving inside the bound releases the wait.
type eventualProber struct{ denials int }

func (p *eventualProber) Capacity(context.Context, RunnerSpec) (bool, error) {
	if p.denials > 0 {
		p.denials--
		return false, nil
	}
	return true, nil
}

func TestCapacityWaitReleasesOnCapacity(t *testing.T) {
	d, _ := newTestDispatcher(t, testConfig(), &fakePodAPI{}, &eventualProber{denials: 3})
	if err := d.waitForCapacity(context.Background(), linuxRunner()); err != nil {
		t.Fatalf("capacity within bound should release the wait, got %v", err)
	}
}

func TestSpecFromEntry(t *testing.T) {
	entry := instance.RunnerEntry{
		Name: "tiny-linux",
		Host: "ghcr.io/goobers/goobers-base:v0.1.0",
		Provides: instance.RunnerProvides{
			OS: instance.RunnerOSLinux, CPU: "2000m", Memory: "4Gi",
		},
		Restrictions: []instance.RunnerRestriction{instance.RunnerRestrictionNetworkAllowlist},
	}
	spec, err := SpecFromEntry(entry)
	if err != nil {
		t.Fatalf("SpecFromEntry: %v", err)
	}
	if spec.HostKind != instance.RunnerHostImage {
		t.Fatalf("HostKind = %q, want image", spec.HostKind)
	}
	if len(spec.Restrictions) != 1 || spec.Restrictions[0] != "network:allowlist" {
		t.Fatalf("Restrictions = %v", spec.Restrictions)
	}
}
