package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type denyNetworkTransport struct {
	attempts atomic.Int32
}

func (t *denyNetworkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.attempts.Add(1)
	return nil, errors.New("network disabled by demo test")
}

type demoDaemonWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	started chan struct{}
	once    sync.Once
}

func (w *demoDaemonWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started != nil && bytes.Contains(p, []byte("daemon started")) {
		w.once.Do(func() { close(w.started) })
	}
	return w.buf.Write(p)
}

func (w *demoDaemonWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestInitDemoBannerGolden(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tour")
	code, stdout, stderr := runArgs(t, "init", "--demo", root)
	if code != 0 {
		t.Fatalf("init --demo: code = %d, stderr = %q", code, stderr)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`initialized instance at %s
  created  instance.yaml
  created  config
  created  runs
  created  scheduler
  created  workcopies
  created  telemetry.db

Demo tour (run these from %s):
  goobers up          # in one terminal
  goobers run demo    # watch stages execute and gate branch
  goobers trace <id>  # see the journal the run left behind
`, abs, abs)
	if stdout != want {
		t.Fatalf("init --demo banner:\n--- got ---\n%s--- want ---\n%s", stdout, want)
	}
}

func TestDemoTourRunsOfflineThroughDaemon(t *testing.T) {
	start := time.Now()
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", "--demo", root); code != 0 {
		t.Fatalf("init --demo: code = %d, stderr = %q", code, stderr)
	}

	var cloneAttempts atomic.Int32
	previousCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) {
		cloneAttempts.Add(1)
		return "", errors.New("repository access disabled by demo test")
	}
	t.Cleanup(func() { repoCloneURL = previousCloneURL })

	networkGuard := &denyNetworkTransport{}
	previousTransport := http.DefaultTransport
	http.DefaultTransport = networkGuard
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	previousSweep := delegationSweepInterval
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { delegationSweepInterval = previousSweep })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upStdout := &demoDaemonWriter{started: make(chan struct{})}
	upStderr := &demoDaemonWriter{}
	upDone := make(chan struct{})
	var upCode int
	go func() {
		upCode = runUpContext(ctx, []string{"--quiet", root}, upStdout, upStderr)
		close(upDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-upDone:
			if upCode != 0 {
				t.Errorf("goobers up: code = %d, stderr = %q", upCode, upStderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("goobers up did not stop")
		}
	})

	select {
	case <-upStdout.started:
	case <-upDone:
		t.Fatalf("goobers up exited before startup: code = %d, stderr = %q", upCode, upStderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for goobers up")
	}

	code, runOut, runErr := runArgs(t, "run", "demo", root)
	if code != 0 {
		t.Fatalf("goobers run demo: code = %d, stdout = %q, stderr = %q", code, runOut, runErr)
	}
	if !strings.Contains(runOut, "phase=completed") {
		t.Fatalf("run output = %q, want completed terminal phase", runOut)
	}
	runID := runIDFromRunStdout(t, runOut)

	code, statusOut, statusErr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("goobers status: code = %d, stderr = %q", code, statusErr)
	}
	if !strings.Contains(statusOut, runID) || !strings.Contains(statusOut, "completed") {
		t.Errorf("status output does not show completed demo run %s:\n%s", runID, statusOut)
	}

	code, traceOut, traceErr := runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("goobers trace: code = %d, stderr = %q", code, traceErr)
	}
	for _, want := range []string{
		"phase:    completed",
		"stage=triage",
		"stage=build",
		"gate=verdict verdict=pass",
	} {
		if !strings.Contains(traceOut, want) {
			t.Errorf("trace output missing %q:\n%s", want, traceOut)
		}
	}
	if strings.Contains(traceOut, "ref.touched") {
		t.Errorf("scratch-only demo recorded a repository ref:\n%s", traceOut)
	}

	if got := cloneAttempts.Load(); got != 0 {
		t.Errorf("demo attempted %d repository operation(s)", got)
	}
	if got := networkGuard.attempts.Load(); got != 0 {
		t.Errorf("demo attempted %d HTTP request(s)", got)
	}
	allOutput := runOut + runErr + statusOut + statusErr + traceOut + traceErr + upStdout.String() + upStderr.String()
	if strings.Contains(strings.ToLower(allOutput), "credential") {
		t.Errorf("demo emitted a credential warning: %q", allOutput)
	}
	entries, err := os.ReadDir(filepath.Join(root, "workcopies", "scratch"))
	if err != nil {
		t.Fatalf("read scratch workspace root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("scratch workspaces were not disposed: %v", entries)
	}
	if elapsed := time.Since(start); elapsed >= time.Minute {
		t.Errorf("demo tour took %s, want under one minute", elapsed)
	}
}
