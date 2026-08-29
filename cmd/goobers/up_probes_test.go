package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// TestUpServesUnauthenticatedProbesOnRealDaemon locks the up.go wiring
// call site for #3806: a real `goobers up` daemon must answer /healthz and
// /readyz with no Authorization header, distinct from and unaffected by the
// existing authenticated /api/v1/health route, and report every named
// readiness subsystem true once startup completes.
func TestUpServesUnauthenticatedProbesOnRealDaemon(t *testing.T) {
	root := initDeterministicDemo(t)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runUpContext(ctx, []string{"--quiet", root}, started, &stderr)
	}()
	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("daemon exited before startup: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// No Authorization header on either request — that is the entire point
	// of #3806: a kubelet probe cannot present one.
	response, err := client.Get("http://" + address + httpapi.LivenessPath)
	if err != nil {
		t.Fatal(err)
	}
	var liveness struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.NewDecoder(response.Body).Decode(&liveness); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !liveness.Healthy {
		t.Fatalf("healthz status = %d, body healthy = %t", response.StatusCode, liveness.Healthy)
	}

	response, err = client.Get("http://" + address + httpapi.ReadinessPath)
	if err != nil {
		t.Fatal(err)
	}
	var readiness httpapi.ReadinessStatus
	if err := json.NewDecoder(response.Body).Decode(&readiness); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !readiness.Ready {
		t.Fatalf("readyz status = %d, ready = %t", response.StatusCode, readiness.Ready)
	}
	for _, check := range []string{"configLoaded", "stateOpen", "resumeComplete", "sweepsStarted"} {
		if !readiness.Checks[check] {
			t.Fatalf("readyz check %q = false once daemon started: checks = %+v", check, readiness.Checks)
		}
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

// startupHoldWriter is the deterministic seam this file's not-ready test
// needs. `goobers up` prints its startup narration to the stdout io.Writer
// its caller hands it, on its own startup goroutine — so a writer that
// blocks inside Write on a chosen line freezes the daemon at exactly that
// point in runUpContextWithForce's sequential body, with the HTTP listener
// already serving. That converts "observe /readyz before the ready gate
// flips" from a race the test has to win (poll fast enough to catch a window
// that is sub-millisecond on an idle demo instance, which is why the polling
// version of this test failed on every CI runner) into a held, deterministic
// observation point.
type startupHoldWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer

	hold    string
	held    chan struct{}
	release chan struct{}
	started chan struct{}

	holdOnce    sync.Once
	releaseOnce sync.Once
	startedOnce sync.Once
}

func newStartupHoldWriter(hold string) *startupHoldWriter {
	return &startupHoldWriter{
		hold:    hold,
		held:    make(chan struct{}),
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
}

func (w *startupHoldWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	w.mu.Unlock()

	if bytes.Contains(p, []byte("daemon started")) {
		w.startedOnce.Do(func() { close(w.started) })
	}
	if bytes.Contains(p, []byte(w.hold)) {
		holder := false
		w.holdOnce.Do(func() {
			holder = true
			close(w.held)
		})
		// Only the first writer of the held line blocks; any later or
		// concurrent write (a resumed run's own narration) passes straight
		// through, so holding startup cannot deadlock another goroutine.
		if holder {
			<-w.release
		}
	}
	return len(p), nil
}

// Release lets the held startup goroutine continue. Idempotent, so a t.Fatalf
// between held and Release still unblocks the daemon via t.Cleanup rather
// than leaking it blocked for the rest of the package's tests.
func (w *startupHoldWriter) Release() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func (w *startupHoldWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// seedInterruptedRun hand-constructs the crash-resume fixture #23's
// TestUpResumesInterruptedRun uses: a run left non-terminal (state.json
// checkpointed at a task, no run.finished event), which the next `goobers up`
// resumes during startup. Its "resuming interrupted run <id>" line is emitted
// after apiServer.Start() and before resumeComplete/sweepsStarted/ready flip,
// which is exactly the window /readyz must report not-ready in.
func seedInterruptedRun(t *testing.T, root, runID string) {
	t.Helper()
	l := instance.NewLayout(root)
	set, report, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatalf("load fixture config: %v (report: %+v)", err, report)
	}
	var wf *apiv1.Workflow
	for i := range set.Workflows {
		if set.Workflows[i].Name == "default-implement" {
			wf = &set.Workflows[i]
		}
	}
	if wf == nil {
		t.Fatal("default-implement workflow not found in fixture config")
	}
	machine, err := workflow.Compile(
		workflow.Definition{Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec},
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile fixture workflow: %v", err)
	}
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        wf.Name,
		WorkflowVersion: 1,
		WorkflowDigest:  machine.Digest(),
		Gaggle:          wf.Spec.Gaggle,
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("hand-construct interrupted run journal: %v", err)
	}
	jr.SetMachineState("local-ci")
	if err := jr.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadyzReportsNotReadyBeforeStartupCompletesOnRealDaemon is the seam
// test the wiring at up.go's WrapWithProbes call site needs and previously
// lacked: TestUpServesUnauthenticatedProbesOnRealDaemon above only observes
// the post-startup happy path, which a hardcoded `Ready: true` (#3806's own
// literal regression) satisfies identically.
//
// It holds a real daemon inside crash-resume — after apiServer.Start() has
// opened the listener, before resumeComplete/sweepsStarted/ready flip — and
// asserts /readyz there: 503, Ready false, and the named checks split exactly
// along the startup point being held (configLoaded/stateOpen already true;
// resumeComplete/sweepsStarted still false). That last split is what makes a
// hardcoded Checks map fail too, not just a hardcoded Ready. /healthz is
// asserted healthy in the same held window, since liveness is deliberately
// decoupled from startup completing — an unbounded crash-resume must not read
// as a wedged main loop.
//
// The hold is a blocking stdout writer (startupHoldWriter above), not a
// timing window: this test's earlier polling form asserted on winning a race
// against a sub-millisecond not-ready window and lost it on every CI runner
// and locally.
func TestReadyzReportsNotReadyBeforeStartupCompletesOnRealDaemon(t *testing.T) {
	const runID = "interrupted-probe-run-1"

	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	seedInterruptedRun(t, root, runID)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdout := newStartupHoldWriter("resuming interrupted run " + runID)
	t.Cleanup(stdout.Release)
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runUpContext(ctx, []string{"--quiet", root}, stdout, &stderr)
	}()

	select {
	case <-stdout.held:
	case code := <-done:
		t.Fatalf("daemon exited before crash-resume: code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon never reached crash-resume: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// Held mid-startup: the listener answers, the ready gate has not flipped.
	response, err := client.Get("http://" + address + httpapi.ReadinessPath)
	if err != nil {
		t.Fatal(err)
	}
	var notReady httpapi.ReadinessStatus
	decodeErr := json.NewDecoder(response.Body).Decode(&notReady)
	notReadyStatus := response.StatusCode
	_ = response.Body.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if notReadyStatus != http.StatusServiceUnavailable || notReady.Ready {
		t.Fatalf("mid-startup /readyz status = %d, ready = %t, want %d / false — the probe must read the live ready gate, not a constant",
			notReadyStatus, notReady.Ready, http.StatusServiceUnavailable)
	}
	for check, want := range map[string]bool{
		"configLoaded":   true,
		"stateOpen":      true,
		"resumeComplete": false,
		"sweepsStarted":  false,
	} {
		if notReady.Checks[check] != want {
			t.Fatalf("mid-startup /readyz check %q = %t, want %t — the named checks must track the startup phase actually reached: checks = %+v",
				check, notReady.Checks[check], want, notReady.Checks)
		}
	}

	// Same window: liveness is decoupled from startup completing.
	response, err = client.Get("http://" + address + httpapi.LivenessPath)
	if err != nil {
		t.Fatal(err)
	}
	var liveness struct {
		Healthy bool `json:"healthy"`
	}
	decodeErr = json.NewDecoder(response.Body).Decode(&liveness)
	livenessStatus := response.StatusCode
	_ = response.Body.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if livenessStatus != http.StatusOK || !liveness.Healthy {
		t.Fatalf("mid-startup /healthz status = %d, healthy = %t — a long crash-resume must not read as a wedged main loop", livenessStatus, liveness.Healthy)
	}

	stdout.Release()

	select {
	case <-stdout.started:
	case code := <-done:
		t.Fatalf("daemon exited: code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for \"daemon started\": stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}

	response, err = client.Get("http://" + address + httpapi.ReadinessPath)
	if err != nil {
		t.Fatal(err)
	}
	var ready httpapi.ReadinessStatus
	decodeErr = json.NewDecoder(response.Body).Decode(&ready)
	readyStatus := response.StatusCode
	_ = response.Body.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if readyStatus != http.StatusOK || !ready.Ready {
		t.Fatalf("post-startup /readyz status = %d, ready = %t, want %d / true", readyStatus, ready.Ready, http.StatusOK)
	}
	for _, check := range []string{"configLoaded", "stateOpen", "resumeComplete", "sweepsStarted"} {
		if !ready.Checks[check] {
			t.Fatalf("post-startup /readyz check %q = false: checks = %+v", check, ready.Checks)
		}
	}

	// Shut down only once the resumed run reaches a terminal phase: the
	// daemon drains in-flight runs indefinitely by default, so cancelling
	// mid-run would hang here rather than exit 0.
	stop := pollUntilRunTerminal(t, filepath.Join(l.RunsDir(), runID), cancel)
	defer stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("daemon did not shut down: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
