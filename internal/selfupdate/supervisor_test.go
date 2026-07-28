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
)

type fakeProcess struct {
	done chan error
	once sync.Once
}

func newFakeProcess() *fakeProcess        { return &fakeProcess{done: make(chan error, 1)} }
func (p *fakeProcess) Done() <-chan error { return p.done }
func (p *fakeProcess) Kill() error        { p.complete(errors.New("killed")); return nil }
func (p *fakeProcess) complete(err error) { p.once.Do(func() { p.done <- err; close(p.done) }) }

type fakeLauncher struct {
	calls   int
	started chan *fakeProcess
	hook    func(int) error
}

func (l *fakeLauncher) Start(string, string, io.Writer, io.Writer) (Process, error) {
	l.calls++
	if l.hook != nil {
		if err := l.hook(l.calls); err != nil {
			return nil, err
		}
	}
	process := newFakeProcess()
	l.started <- process
	return process, nil
}

type fakeEscalator struct{ calls chan Request }

func (e fakeEscalator) Escalate(_ context.Context, request Request, _ string) error {
	e.calls <- request
	return nil
}

func TestSupervisorPromotesHealthyCandidate(t *testing.T) {
	root, now, _ := setupSupervisorRequest(t)
	lockPath := filepath.Join(root, "scheduler", "up.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestExecutable(t, lockPath, "lock")
	if err := os.Chtimes(lockPath, now.Add(-time.Second), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 3)}
	launcher.hook = func(call int) error {
		if call == 2 {
			return os.Chtimes(lockPath, now.Add(time.Second), now.Add(time.Second))
		}
		return nil
	}
	cancel, done := startSupervisor(root, launcher, fakeEscalator{make(chan Request, 1)})
	old := <-launcher.started
	drainAndComplete(t, root, old)
	candidate := <-launcher.started
	time.Sleep(30 * time.Millisecond)
	if err := os.Chtimes(lockPath, now.Add(2*time.Second), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, err := os.Stat(RequestPath(root))
		return errors.Is(err, os.ErrNotExist)
	})
	if got, _ := os.ReadFile(CurrentBinary(root, "linux")); string(got) != "candidate" {
		t.Fatalf("current binary = %q", got)
	}
	if got, _ := os.ReadFile(PreviousBinary(root, "linux")); string(got) != "old" {
		t.Fatalf("previous binary = %q", got)
	}
	stopSupervisor(t, root, cancel, candidate, done)
}

func TestSupervisorRollsBackAndEscalatesBrokenCandidate(t *testing.T) {
	root, _, _ := setupSupervisorRequest(t)
	escalations := make(chan Request, 1)
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 3)}
	cancel, done := startSupervisor(root, launcher, fakeEscalator{escalations})
	old := <-launcher.started
	drainAndComplete(t, root, old)
	candidate := <-launcher.started
	candidate.complete(errors.New("broken candidate"))
	restored := <-launcher.started
	select {
	case <-escalations:
	case <-time.After(time.Second):
		t.Fatal("rollback was not escalated")
	}
	if got, _ := os.ReadFile(CurrentBinary(root, "linux")); string(got) != "old" {
		t.Fatalf("rolled-back binary = %q", got)
	}
	stopSupervisor(t, root, cancel, restored, done)
}

func TestSupervisorKeepsCurrentAfterConsecutiveActivationCrash(t *testing.T) {
	root, _, request := setupSupervisorRequest(t)
	writeTestExecutable(t, PreviousBinary(root, "linux"), "stale")
	request.Status, request.Target = "activating", "v3"
	if err := WriteRequest(root, request); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 1)}
	cancel, done := startSupervisor(root, launcher, fakeEscalator{make(chan Request, 1)})
	current := <-launcher.started
	if got, _ := os.ReadFile(CurrentBinary(root, "linux")); string(got) != "old" {
		t.Fatalf("current binary after interrupted second activation = %q", got)
	}
	stopSupervisor(t, root, cancel, current, done)
}
func setupSupervisorRequest(t *testing.T) (string, time.Time, Request) {
	t.Helper()
	root := t.TempDir()
	writeTestExecutable(t, CurrentBinary(root, "linux"), "old")
	staged := filepath.Join(StagingDir(root), "target", "goobers")
	writeTestExecutable(t, staged, "candidate")
	now := time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC)
	request := Request{
		RunID: "run", Policy: PolicyOnRelease, Owner: "acme", Repository: "goobers", Target: "v2", StagedPath: staged, RequestedAt: now,
		HealthTicks: 1, HealthTimeout: time.Minute.String(), Status: "requested",
	}
	if err := WriteRequest(root, request); err != nil {
		t.Fatal(err)
	}
	return root, now, request
}

func startSupervisor(root string, launcher Launcher, escalator Escalator) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, GOOS: "linux",
			Launcher: launcher, Escalator: escalator, PollInterval: 5 * time.Millisecond,
			DrainTimeout: 100 * time.Millisecond,
		})
	}()
	return cancel, done
}

func drainAndComplete(t *testing.T, root string, process *fakeProcess) {
	t.Helper()
	waitFor(t, func() bool {
		_, err := os.Stat(StopRequestPath(root))
		return err == nil
	})
	if _, err := ConsumeStopRequest(root); err != nil {
		t.Fatal(err)
	}
	process.complete(nil)
}

func stopSupervisor(t *testing.T, root string, cancel context.CancelFunc, process *fakeProcess, done <-chan error) {
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
			t.Fatal(err)
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
