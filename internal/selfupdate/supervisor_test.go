package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	drainAndComplete(t, root, done, old)
	candidate := <-launcher.started
	heartbeat := now.Add(time.Second)
	// Keep advancing heartbeats until the supervisor observes two distinct ticks.
	waitForSupervisor(t, done, "the healthy candidate was promoted and its request retired", func() bool {
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
	drainAndComplete(t, root, done, old)
	candidate := <-launcher.started
	candidate.complete(errors.New("broken candidate"))
	restored := <-launcher.started
	waitForSupervisor(t, done, "both rollback escalations were attempted", func() bool { return len(escalations) == 2 })
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
func drainAndComplete(t *testing.T, root string, done <-chan error, process *fakeProcess) {
	t.Helper()
	waitForSupervisor(t, done, "the supervisor requested the daemon drain for the handoff", func() bool {
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
	// Not waitForSupervisor: the supervisor is EXPECTED to exit here, and its
	// result is read below rather than treated as a lost race.
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

// waitForBudget bounds how long a supervisor step is waited on.
//
// It used to be one second, and the length was doing a job it could not do:
// the only thing that ever stops these conditions from being met is the
// supervisor goroutine failing or exiting, and a short budget was the way that
// got noticed. It noticed CPU starvation just as readily. Under `make ci` —
// every package at once, under -race and coverage — a second of scheduling
// delay is an ordinary event on a healthy machine, and the test reported it as
// "condition was not met", naming neither what was waited on nor why (#3234).
//
// waitForSupervisor below now watches the supervisor's own result channel, so a
// supervisor that failed is reported IMMEDIATELY, with its error, rather than
// inferred from a clock. That leaves this budget covering nothing but
// scheduling delay, which is not a defect and should not be measured; it is set
// far above any plausible delay and bounded in the end by the package's own
// `go test -timeout`.
const waitForBudget = 60 * time.Second

// waitForPoll is how often a condition is re-read. The supervisor's own poll
// interval in these tests is 5ms, so this is fast enough to see every state it
// passes through without spinning.
const waitForPoll = time.Millisecond

// waitFor polls condition without watching a supervisor. Prefer
// waitForSupervisor wherever the condition depends on one: a failed supervisor
// makes such a condition unreachable, and only that form can say so.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	waitForSupervisor(t, nil, "", condition)
}

// waitForSupervisor polls condition until it holds, the supervisor exits, or
// the budget runs out.
//
// Watching done is the point. Every condition these tests wait on is an effect
// the supervisor produces, so a supervisor that has already returned an error
// makes the condition permanently unreachable — and the previous helper
// answered that with a timeout and the words "condition was not met", which
// name the symptom and hide the cause. Reading the result channel turns that
// into the supervisor's own error, at the moment it happens.
func waitForSupervisor(t *testing.T, done <-chan error, what string, condition func() bool) {
	t.Helper()
	if err := awaitSupervisorState(done, what, waitForBudget, condition); err != nil {
		t.Fatal(err)
	}
}

// awaitSupervisorState is waitForSupervisor's decision, split from the
// reporting so it can be tested for the thing it exists to do: answering with
// the supervisor's failure rather than with a clock.
func awaitSupervisorState(done <-chan error, what string, budget time.Duration, condition func() bool) error {
	if what == "" {
		what = "the awaited supervisor state"
	}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if condition() {
			return nil
		}
		select {
		case err := <-done:
			// The supervisor is gone, so nothing will ever satisfy the
			// condition. Re-check once first: it may have finished the work
			// and exited between the poll above and this read.
			if condition() {
				return nil
			}
			if err == nil {
				return fmt.Errorf("the supervisor exited cleanly before %s; nothing remains to produce it", what)
			}
			return fmt.Errorf("the supervisor exited before %s; nothing remains to produce it: %w", what, err)
		default:
		}
		time.Sleep(waitForPoll)
	}
	return fmt.Errorf("%s did not happen within %s", what, budget)
}

// #3234: a supervisor that has already failed must be REPORTED, not waited out.
//
// The reported flake was "condition was not met" — a bare timeout that named
// neither what was awaited nor why it never arrived, which is all the previous
// helper could say for either cause it conflated: a supervisor that had failed,
// and a goroutine that had merely not been scheduled yet under a saturated
// `make ci`. Only the first is a defect, and it is the one the test could not
// distinguish.
func TestAwaitSupervisorStateReportsTheSupervisorsOwnFailure(t *testing.T) {
	done := make(chan error, 1)
	done <- errors.New("start supervised daemon: no such binary")

	err := awaitSupervisorState(done, "the candidate was promoted", time.Minute, func() bool { return false })
	if err == nil {
		t.Fatal("awaitSupervisorState returned nil for a supervisor that had already failed")
	}
	if !strings.Contains(err.Error(), "no such binary") {
		t.Fatalf("error = %v, want the supervisor's own failure; a bare timeout hides the cause", err)
	}
	if !strings.Contains(err.Error(), "the candidate was promoted") {
		t.Fatalf("error = %v, want the awaited state named", err)
	}
}

// A supervisor that finishes the work and exits in the same breath must not be
// reported as having exited early: the condition it produced is the answer.
func TestAwaitSupervisorStateAcceptsWorkFinishedAsTheSupervisorExits(t *testing.T) {
	done := make(chan error, 1)
	done <- nil
	satisfied := false
	err := awaitSupervisorState(done, "the request was retired", time.Minute, func() bool {
		// False on the first poll, true on the re-check that follows the
		// result read — the interleaving the re-check exists for.
		defer func() { satisfied = true }()
		return satisfied
	})
	if err != nil {
		t.Fatalf("awaitSupervisorState = %v, want nil: the awaited state was reached", err)
	}
}

// And a condition that simply never arrives still ends, naming what was awaited.
func TestAwaitSupervisorStateTimesOutNamingTheAwaitedState(t *testing.T) {
	err := awaitSupervisorState(nil, "the candidate was promoted", 20*time.Millisecond, func() bool { return false })
	if err == nil || !strings.Contains(err.Error(), "the candidate was promoted") {
		t.Fatalf("error = %v, want a timeout naming the awaited state", err)
	}
}
