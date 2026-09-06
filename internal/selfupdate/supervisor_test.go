package selfupdate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type staticVersionRunner struct {
	info versionInfo
	err  error
}

func (r staticVersionRunner) Run(context.Context, string, []string, string, ...string) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	return []byte(`{"version":"` + r.info.Version + `","commit":"` + r.info.Commit + `"}`), nil
}

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

func (l *fakeLauncher) Start(string, string, io.Writer, io.Writer) (process, error) {
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

type escalatorFunc func(context.Context, Request, string) error

func (f escalatorFunc) Escalate(ctx context.Context, request Request, reason string) error {
	return f(ctx, request, reason)
}

func TestEnsureCurrentBinaryRefreshesOlderVersion(t *testing.T) {
	root := t.TempDir()
	current := currentBinary(root, "linux")
	installed := filepath.Join(root, "installed", "goobers")
	writeTestExecutable(t, current, "old")
	writeTestExecutable(t, installed, "new")
	var stderr strings.Builder

	err := ensureCurrentBinary(defaultSupervisorOptions(SupervisorOptions{
		Root: root, GOOS: "linux", Stderr: &stderr,
		runner:     staticVersionRunner{info: versionInfo{Version: "v1.2.3", Commit: "old"}},
		executable: installed,
		supervisor: versionInfo{Version: "v1.3.0", Commit: "new"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(current); err != nil || string(got) != "new" {
		t.Fatalf("current binary = %q, %v; want installed binary", got, err)
	}
	if got := stderr.String(); !strings.Contains(got, "refreshed supervised daemon from v1.2.3") {
		t.Fatalf("stderr = %q, want refresh diagnosis", got)
	}
}

func TestEnsureCurrentBinaryDoesNotReplaceNewerActiveVersion(t *testing.T) {
	root := t.TempDir()
	current := currentBinary(root, "linux")
	installed := filepath.Join(root, "installed", "goobers")
	writeTestExecutable(t, current, "new")
	writeTestExecutable(t, installed, "old")
	var stderr strings.Builder

	err := ensureCurrentBinary(defaultSupervisorOptions(SupervisorOptions{
		Root: root, GOOS: "linux", Stderr: &stderr,
		runner:     staticVersionRunner{info: versionInfo{Version: "v1.3.0", Commit: "new"}},
		executable: installed,
		supervisor: versionInfo{Version: "v1.2.3", Commit: "old"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(current); err != nil || string(got) != "new" {
		t.Fatalf("current binary = %q, %v; want newer active binary", got, err)
	}
	if got := stderr.String(); !strings.Contains(got, "keeping existing binary") {
		t.Fatalf("stderr = %q, want skew diagnosis", got)
	}
}

func TestDaemonOutputMergesAndRedactsStreams(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr strings.Builder
	output, err := openDaemonOutput(root, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := output.Close(); err != nil {
			t.Errorf("close daemon output: %v", err)
		}
	})

	stdoutWriter := output.child(&stdout)
	stderrWriter := output.child(&stderr)
	if _, err := stdoutWriter.Write([]byte("startup: scheduler initialized\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderrWriter.Write([]byte("Authorization: Bearer ghp_123456789012345678901234567890123456\n")); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(root, "scheduler", "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	logged := string(data)
	if !strings.Contains(logged, "scheduler initialized") || !strings.Contains(logged, "REDACTED") {
		t.Fatalf("daemon log = %q, want merged startup and redacted error output", logged)
	}
	if strings.Contains(logged, "ghp_123456789012345678901234567890123456") {
		t.Fatalf("daemon log contains bearer token: %q", logged)
	}
	if !strings.Contains(stdout.String(), "scheduler initialized") || strings.Contains(stdout.String(), "Authorization") {
		t.Fatalf("foreground stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "REDACTED") {
		t.Fatalf("foreground stderr = %q, want redacted output", stderr.String())
	}
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
	heartbeat := now.Add(time.Second)
	// Keep advancing heartbeats until the supervisor observes two distinct ticks.
	waitFor(t, func() bool {
		heartbeat = heartbeat.Add(time.Second)
		if err := os.Chtimes(lockPath, heartbeat, heartbeat); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(requestPath(root))
		return errors.Is(err, os.ErrNotExist)
	})
	if got, _ := os.ReadFile(currentBinary(root, "linux")); string(got) != "candidate" {
		t.Fatalf("current binary = %q", got)
	}
	if got, _ := os.ReadFile(previousBinary(root, "linux")); string(got) != "old" {
		t.Fatalf("previous binary = %q", got)
	}
	stopSupervisor(t, root, cancel, candidate, done)
}

func TestSupervisorRollsBackAndEscalatesBrokenCandidate(t *testing.T) {
	root, _, _ := setupSupervisorRequest(t)
	escalations := make(chan Request, 2)
	results := make(chan error, 1)
	results <- errors.New("escalation provider unavailable")
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 3)}
	cancel, done := startSupervisor(root, launcher, escalatorFunc(func(_ context.Context, request Request, _ string) error {
		escalations <- request
		return <-results
	}))
	old := <-launcher.started
	drainAndComplete(t, root, old)
	candidate := <-launcher.started
	candidate.complete(errors.New("broken candidate"))
	restored := <-launcher.started
	waitFor(t, func() bool { return len(escalations) == 2 })
	if got, _ := os.ReadFile(currentBinary(root, "linux")); string(got) != "old" {
		t.Fatalf("rolled-back binary = %q", got)
	}
	results <- nil
	stopSupervisor(t, root, cancel, restored, done)
}

func TestSupervisorKeepsCurrentAfterConsecutiveActivationCrash(t *testing.T) {
	root, _, request := setupSupervisorRequest(t)
	writeTestExecutable(t, previousBinary(root, "linux"), "stale")
	request.Status, request.Target = "activating", "v3"
	if err := writeRequest(root, request); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{started: make(chan *fakeProcess, 1)}
	cancel, done := startSupervisor(root, launcher, fakeEscalator{make(chan Request, 1)})
	current := <-launcher.started
	if got, _ := os.ReadFile(currentBinary(root, "linux")); string(got) != "old" {
		t.Fatalf("current binary after interrupted second activation = %q", got)
	}
	stopSupervisor(t, root, cancel, current, done)
}

func setupSupervisorRequest(t *testing.T) (string, time.Time, Request) {
	t.Helper()
	root := t.TempDir()
	writeTestExecutable(t, currentBinary(root, "linux"), "old")
	staged := filepath.Join(stagingDir(root), "target", "goobers")
	writeTestExecutable(t, staged, "candidate")
	now := time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC)
	request := Request{
		RunID: "run", Policy: PolicyOnRelease, Owner: "acme", Repository: "goobers", Target: "v2", StagedPath: staged, RequestedAt: now,
		HealthTicks: 1, HealthTimeout: time.Minute.String(), Status: "requested",
	}
	if err := writeRequest(root, request); err != nil {
		t.Fatal(err)
	}
	return root, now, request
}

func startSupervisor(root string, launcher launcher, escalator escalator) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, GOOS: "linux",
			Launcher: launcher, Escalator: escalator, PollInterval: 5 * time.Millisecond,
			DrainTimeout: 100 * time.Millisecond,
			runner:       staticVersionRunner{info: versionInfo{Version: "v1", Commit: "old"}},
			supervisor:   versionInfo{Version: "v1", Commit: "old"},
		})
	}()
	return cancel, done
}
func drainAndComplete(t *testing.T, root string, process *fakeProcess) {
	t.Helper()
	waitFor(t, func() bool {
		_, err := os.Stat(stopRequestPath(root))
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
		_, err := os.Stat(stopRequestPath(root))
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

// waitForBudget is 1s on POSIX; Windows CI runners are measurably slower at
// the file-mtime polling and process spawning this package's supervisor loop
// does (ci.yml's windows-smoke job documents the same finding for the
// package-level `go test` timeout), so give the loop more real time to catch
// up there rather than tightening the flake margin.
func waitForBudget() time.Duration {
	if runtime.GOOS == "windows" {
		return 5 * time.Second
	}
	return time.Second
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitForBudget())
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond) // Polling interval for synchronized supervisor state.
	}
	t.Fatal("condition was not met")
}
