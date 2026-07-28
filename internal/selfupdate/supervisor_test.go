package selfupdate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type fakeProcess struct {
	done chan error
	once sync.Once
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{done: make(chan error, 1)}
}

func (p *fakeProcess) Done() <-chan error { return p.done }
func (p *fakeProcess) Kill() error {
	p.complete(errors.New("killed"))
	return nil
}
func (p *fakeProcess) complete(err error) {
	p.once.Do(func() {
		p.done <- err
		close(p.done)
	})
}

type fakeLauncher struct {
	mu          sync.Mutex
	calls       int
	startErrors map[int]error
	startHooks  map[int]func() error
	started     chan *fakeProcess
}

func (l *fakeLauncher) Start(string, string, io.Writer, io.Writer) (Process, error) {
	l.mu.Lock()
	l.calls++
	err := l.startErrors[l.calls]
	hook := l.startHooks[l.calls]
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if hook != nil {
		if err := hook(); err != nil {
			return nil, err
		}
	}
	process := newFakeProcess()
	l.started <- process
	return process, nil
}

type fakeEscalator struct {
	calls chan Request
}

func (e *fakeEscalator) Escalate(_ context.Context, request Request, _ string) error {
	e.calls <- request
	return nil
}

func TestSupervisorPromotesCandidateAfterCleanHeartbeat(t *testing.T) {
	root, now := setupSupervisorRequest(t)
	lockPath := filepath.Join(root, "scheduler", "up.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	heartbeat := now.Add(-time.Second)
	if err := os.Chtimes(lockPath, heartbeat, heartbeat); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{
		started: make(chan *fakeProcess, 4),
		startHooks: map[int]func() error{
			2: func() error {
				acquired := now.Add(time.Second)
				return os.Chtimes(lockPath, acquired, acquired)
			},
		},
	}
	escalator := &fakeEscalator{calls: make(chan Request, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, HostExecutable: CurrentBinary(root, "linux"), GOOS: "linux",
			Launcher: launcher, Escalator: escalator, PollInterval: 5 * time.Millisecond,
			DrainTimeout: 100 * time.Millisecond, Now: func() time.Time { return now },
		})
	}()

	old := <-launcher.started
	waitFor(t, func() bool {
		_, err := os.Stat(StopRequestPath(root))
		return err == nil
	})
	if _, err := ConsumeStopRequest(root); err != nil {
		t.Fatal(err)
	}
	old.complete(nil)
	candidate := <-launcher.started
	waitFor(t, func() bool {
		return hasUpdateEvent(t, root, journal.EventDaemonUpdateRestarted)
	})

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(RequestPath(root)); err != nil {
		t.Fatalf("candidate accepted from lock acquisition without a scheduler heartbeat: %v", err)
	}
	heartbeat = now.Add(2 * time.Second)
	if err := os.Chtimes(lockPath, heartbeat, heartbeat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, err := os.Stat(RequestPath(root))
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(filepath.Join(StagingDir(root), "target")); !os.IsNotExist(err) {
		t.Fatalf("completed staging directory still exists: %v", err)
	}
	if got, err := os.ReadFile(CurrentBinary(root, "linux")); err != nil || string(got) != "candidate" {
		t.Fatalf("current binary = %q, %v", got, err)
	}
	if got, err := os.ReadFile(PreviousBinary(root, "linux")); err != nil || string(got) != "old" {
		t.Fatalf("previous binary = %q, %v", got, err)
	}
	assertUpdateEvent(t, root, journal.EventDaemonUpdateDrainStarted)
	assertUpdateEvent(t, root, journal.EventDaemonUpdateRestarted)
	assertUpdateEvent(t, root, journal.EventDaemonUpdateHealthy)
	stopFakeSupervisor(t, root, cancel, candidate, done)
}

func TestSupervisorRollsBackBrokenCandidateAndEscalates(t *testing.T) {
	root, now := setupSupervisorRequest(t)
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 4)}
	escalator := &fakeEscalator{calls: make(chan Request, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, HostExecutable: CurrentBinary(root, "linux"), GOOS: "linux",
			Launcher: launcher, Escalator: escalator, PollInterval: 5 * time.Millisecond,
			DrainTimeout: 100 * time.Millisecond, Now: func() time.Time { return now },
		})
	}()

	old := <-launcher.started
	waitFor(t, func() bool {
		_, err := os.Stat(StopRequestPath(root))
		return err == nil
	})
	if _, err := ConsumeStopRequest(root); err != nil {
		t.Fatal(err)
	}
	old.complete(nil)
	candidate := <-launcher.started
	candidate.complete(errors.New("broken candidate"))
	restored := <-launcher.started
	select {
	case request := <-escalator.calls:
		if request.Target != "v2" || request.Reason == "" {
			t.Fatalf("escalation request = %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("rollback escalation was not created")
	}
	if got, err := os.ReadFile(CurrentBinary(root, "linux")); err != nil || string(got) != "old" {
		t.Fatalf("rolled-back binary = %q, %v", got, err)
	}
	waitFor(t, func() bool {
		_, err := os.Stat(RequestPath(root))
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(filepath.Join(StagingDir(root), "target")); !os.IsNotExist(err) {
		t.Fatalf("rolled-back staging directory still exists: %v", err)
	}
	assertUpdateEvent(t, root, journal.EventDaemonUpdateRolledBack)
	assertUpdateEvent(t, root, journal.EventDaemonUpdateEscalated)
	stopFakeSupervisor(t, root, cancel, restored, done)
}

func TestSupervisorDoesNotJournalRestartWhenCandidateLaunchFails(t *testing.T) {
	root, now := setupSupervisorRequest(t)
	launcher := &fakeLauncher{
		started:     make(chan *fakeProcess, 4),
		startErrors: map[int]error{2: errors.New("candidate launch failed")},
	}
	escalator := &fakeEscalator{calls: make(chan Request, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, HostExecutable: CurrentBinary(root, "linux"), GOOS: "linux",
			Launcher: launcher, Escalator: escalator, PollInterval: 5 * time.Millisecond,
			DrainTimeout: 100 * time.Millisecond, Now: func() time.Time { return now },
		})
	}()

	old := <-launcher.started
	waitFor(t, func() bool {
		_, err := os.Stat(StopRequestPath(root))
		return err == nil
	})
	if _, err := ConsumeStopRequest(root); err != nil {
		t.Fatal(err)
	}
	old.complete(nil)
	restored := <-launcher.started
	select {
	case <-escalator.calls:
	case <-time.After(time.Second):
		t.Fatal("rollback escalation was not created")
	}
	assertUpdateEvent(t, root, journal.EventDaemonUpdateRolledBack)
	assertNoUpdateEvent(t, root, journal.EventDaemonUpdateRestarted)
	stopFakeSupervisor(t, root, cancel, restored, done)
}

func TestSupervisorQuarantinesInvalidRequestAndKeepsDaemonRunning(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "linux")
	writeTestExecutable(t, current, []byte("old"))
	if err := os.MkdirAll(UpdatesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RequestPath(root), []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, HostExecutable: current, GOOS: "linux", Launcher: launcher,
			PollInterval: 5 * time.Millisecond, DrainTimeout: 100 * time.Millisecond,
		})
	}()
	process := <-launcher.started
	matches, err := filepath.Glob(RequestPath(root) + ".invalid.*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantined requests = %v, %v", matches, err)
	}
	assertUpdateEvent(t, root, journal.EventError)
	stopFakeSupervisor(t, root, cancel, process, done)
}

func TestActivateCandidateDoesNotOverwritePreviousAfterActivationCrash(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "linux")
	previous := PreviousBinary(root, "linux")
	staged := filepath.Join(StagingDir(root), "target", "goobers")
	writeTestExecutable(t, current, []byte("candidate"))
	writeTestExecutable(t, staged, []byte("candidate"))
	writeTestExecutable(t, previous, []byte("old"))
	if err := activateCandidate(SupervisorOptions{Root: root, GOOS: "linux"}, Request{StagedPath: staged}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(previous); err != nil || string(got) != "old" {
		t.Fatalf("previous binary = %q, %v", got, err)
	}
}

func TestActivateCandidatePreservesPreviousAcrossInterruptedSecondUpdate(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "windows")
	previous := PreviousBinary(root, "windows")
	staged := filepath.Join(StagingDir(root), "v2", "goobers.exe")
	writeTestExecutable(t, current, []byte("v1"))
	writeTestExecutable(t, previous, []byte("v0"))
	writeTestExecutable(t, staged, []byte("v2"))
	request := Request{StagedPath: staged}

	copies := 0
	err := activateCandidateWithCopier(SupervisorOptions{Root: root, GOOS: "windows"}, request, func(source, destination string) error {
		copies++
		if copies == 2 {
			return errors.New("replacement interrupted")
		}
		return copyExecutable(source, destination)
	})
	if err == nil {
		t.Fatal("activateCandidateWithCopier succeeded after interrupted replacement")
	}
	if got, readErr := os.ReadFile(current); readErr != nil || string(got) != "v1" {
		t.Fatalf("current binary after interruption = %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(previous); readErr != nil || string(got) != "v1" {
		t.Fatalf("previous binary after interruption = %q, %v", got, readErr)
	}

	if err := activateCandidate(SupervisorOptions{Root: root, GOOS: "windows"}, request); err != nil {
		t.Fatal(err)
	}
	if err := restorePrevious(SupervisorOptions{Root: root, GOOS: "windows"}); err != nil {
		t.Fatal(err)
	}
	if got, readErr := os.ReadFile(current); readErr != nil || string(got) != "v1" {
		t.Fatalf("rolled-back binary after retry = %q, %v", got, readErr)
	}
}

func TestSupervisorFinalizesHealthyRequestAfterRestart(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "linux")
	previous := PreviousBinary(root, "linux")
	staged := filepath.Join(StagingDir(root), "target", "goobers")
	writeTestExecutable(t, current, []byte("candidate"))
	writeTestExecutable(t, previous, []byte("old"))
	writeTestExecutable(t, staged, []byte("candidate"))
	request := Request{
		RunID: "run-healthy", Policy: PolicyOnRelease, Owner: "acme", Repository: "goobers",
		Target: "v2", Version: "v2", StagedPath: staged, RequestedAt: time.Now().UTC(),
		HealthTicks: 1, HealthTimeout: time.Minute.String(), HeartbeatInterval: time.Second.String(), Status: "healthy",
	}
	if err := WriteRequest(root, request); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, HostExecutable: current, GOOS: "linux", Launcher: launcher,
			PollInterval: 5 * time.Millisecond, DrainTimeout: 100 * time.Millisecond,
		})
	}()
	process := <-launcher.started
	if got, err := os.ReadFile(current); err != nil || string(got) != "candidate" {
		t.Fatalf("current binary = %q, %v", got, err)
	}
	if _, err := os.Stat(RequestPath(root)); !os.IsNotExist(err) {
		t.Fatalf("healthy request still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(staged)); !os.IsNotExist(err) {
		t.Fatalf("healthy staging still exists: %v", err)
	}
	assertUpdateEvent(t, root, journal.EventDaemonUpdateHealthy)
	assertNoUpdateEvent(t, root, journal.EventDaemonUpdateRolledBack)
	stopFakeSupervisor(t, root, cancel, process, done)
}

func TestSupervisorDoesNotDuplicateJournaledHealthyEventAfterRestart(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "linux")
	staged := filepath.Join(StagingDir(root), "target", "goobers")
	writeTestExecutable(t, current, []byte("candidate"))
	writeTestExecutable(t, staged, []byte("candidate"))
	request := Request{
		RunID: "run-journaled-healthy", Policy: PolicyOnRelease, Owner: "acme", Repository: "goobers",
		Target: "v2", Version: "v2", StagedPath: staged, RequestedAt: time.Now().UTC(),
		HealthTicks: 1, HealthTimeout: time.Minute.String(), HeartbeatInterval: time.Second.String(), Status: "healthy",
	}
	if err := WriteRequest(root, request); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err := appendUpdateEvent(log, journal.EventDaemonUpdateHealthy, request, "candidate completed clean heartbeat window"); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, HostExecutable: current, GOOS: "linux", Launcher: launcher,
			PollInterval: 5 * time.Millisecond, DrainTimeout: 100 * time.Millisecond,
		})
	}()
	process := <-launcher.started
	events, err := journal.ReadInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	healthy := 0
	for _, event := range events {
		if event.Type == journal.EventDaemonUpdateHealthy {
			healthy++
		}
	}
	if healthy != 1 {
		t.Fatalf("healthy events = %d, want 1", healthy)
	}
	assertNoUpdateEvent(t, root, journal.EventDaemonUpdateRolledBack)
	stopFakeSupervisor(t, root, cancel, process, done)
}

func TestSupervisorRepairsMissingRollbackEventAfterRestart(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "linux")
	previous := PreviousBinary(root, "linux")
	staged := filepath.Join(StagingDir(root), "target", "goobers")
	writeTestExecutable(t, current, []byte("candidate"))
	writeTestExecutable(t, previous, []byte("old"))
	writeTestExecutable(t, staged, []byte("candidate"))
	request := Request{
		RunID: "run-rollback", Policy: PolicyOnRelease, Owner: "acme", Repository: "goobers",
		Target: "v2", Version: "v2", StagedPath: staged, RequestedAt: time.Now().UTC(),
		HealthTicks: 1, HealthTimeout: time.Minute.String(), HeartbeatInterval: time.Second.String(),
		Status: "rollback", Reason: "candidate failed health check",
	}
	if err := WriteRequest(root, request); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 1)}
	escalator := &fakeEscalator{calls: make(chan Request, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, HostExecutable: current, GOOS: "linux", Launcher: launcher, Escalator: escalator,
			PollInterval: 5 * time.Millisecond, DrainTimeout: 100 * time.Millisecond,
		})
	}()
	process := <-launcher.started
	if got, err := os.ReadFile(current); err != nil || string(got) != "old" {
		t.Fatalf("restored binary = %q, %v", got, err)
	}
	assertUpdateEvent(t, root, journal.EventDaemonUpdateRolledBack)
	select {
	case <-escalator.calls:
	case <-time.After(time.Second):
		t.Fatal("rollback escalation was not created")
	}
	stopFakeSupervisor(t, root, cancel, process, done)
}

func TestSupervisorDoesNotDuplicateRollbackEventAfterRestart(t *testing.T) {
	root := t.TempDir()
	current := CurrentBinary(root, "linux")
	previous := PreviousBinary(root, "linux")
	staged := filepath.Join(StagingDir(root), "target", "goobers")
	writeTestExecutable(t, current, []byte("candidate"))
	writeTestExecutable(t, previous, []byte("old"))
	writeTestExecutable(t, staged, []byte("candidate"))
	request := Request{
		RunID: "run-journaled-rollback", Policy: PolicyOnRelease, Owner: "acme", Repository: "goobers",
		Target: "v2", Version: "v2", StagedPath: staged, RequestedAt: time.Now().UTC(),
		HealthTicks: 1, HealthTimeout: time.Minute.String(), HeartbeatInterval: time.Second.String(),
		Status: "rollback", Reason: "candidate failed health check",
	}
	if err := WriteRequest(root, request); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err := appendUpdateEvent(log, journal.EventDaemonUpdateRolledBack, request, request.Reason); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 1)}
	escalator := &fakeEscalator{calls: make(chan Request, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, HostExecutable: current, GOOS: "linux", Launcher: launcher, Escalator: escalator,
			PollInterval: 5 * time.Millisecond, DrainTimeout: 100 * time.Millisecond,
		})
	}()
	process := <-launcher.started
	events, err := journal.ReadInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	rolledBack := 0
	for _, event := range events {
		if event.Type == journal.EventDaemonUpdateRolledBack {
			rolledBack++
		}
	}
	if rolledBack != 1 {
		t.Fatalf("rollback events = %d, want 1", rolledBack)
	}
	stopFakeSupervisor(t, root, cancel, process, done)
}

func setupSupervisorRequest(t *testing.T) (string, time.Time) {
	t.Helper()
	root := t.TempDir()
	writeTestExecutable(t, CurrentBinary(root, "linux"), []byte("old"))
	staged := filepath.Join(StagingDir(root), "target", "goobers")
	writeTestExecutable(t, staged, []byte("candidate"))
	now := time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC)
	if err := WriteRequest(root, Request{
		RunID: "run-2", Policy: PolicyOnRelease, Owner: "acme", Repository: "goobers",
		Target: "v2", Version: "v2", StagedPath: staged, RequestedAt: now,
		HealthTicks: 1, HealthTimeout: time.Minute.String(), HeartbeatInterval: time.Second.String(), Status: "requested",
	}); err != nil {
		t.Fatal(err)
	}
	return root, now
}

func stopFakeSupervisor(t *testing.T, root string, cancel context.CancelFunc, process *fakeProcess, done <-chan error) {
	t.Helper()
	cancel()
	waitFor(t, func() bool {
		_, err := os.Stat(StopRequestPath(root))
		return err == nil
	})
	process.complete(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSupervisor: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func assertUpdateEvent(t *testing.T, root string, want journal.EventType) {
	t.Helper()
	if hasUpdateEvent(t, root, want) {
		return
	}
	events, err := journal.ReadInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("events missing %s: %+v", want, events)
}

func assertNoUpdateEvent(t *testing.T, root string, unwanted journal.EventType) {
	t.Helper()
	if hasUpdateEvent(t, root, unwanted) {
		t.Fatalf("events unexpectedly contain %s", unwanted)
	}
}

func hasUpdateEvent(t *testing.T, root string, want journal.EventType) bool {
	t.Helper()
	events, err := journal.ReadInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == want {
			return true
		}
	}
	return false
}
