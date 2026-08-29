package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
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

// TestReadyzReportsNotReadyBeforeStartupCompletesOnRealDaemon is the seam
// test the wiring at up.go's WrapWithProbes call site needs and previously
// lacked: TestUpServesUnauthenticatedProbesOnRealDaemon above only observes
// the post-startup happy path, which a hardcoded `Ready: true` (#3806's own
// literal regression) satisfies identically. This test polls /readyz from
// the moment the HTTP listener opens — before "daemon started" fires, i.e.
// before webhookGate.Start() flips the ready gate — and requires observing
// at least one genuinely not-ready response (503, ready=false) before the
// first ready=true one, then asserts every named check is true once ready.
func TestReadyzReportsNotReadyBeforeStartupCompletesOnRealDaemon(t *testing.T) {
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

	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + address + httpapi.ReadinessPath

	sawNotReady := false
	deadline := time.Now().Add(20 * time.Second)
	var final httpapi.ReadinessStatus
	reachedReady := false
	for time.Now().Before(deadline) && !reachedReady {
		select {
		case code := <-done:
			t.Fatalf("daemon exited before becoming ready: code = %d, stderr = %q", code, stderr.String())
		default:
		}

		response, err := client.Get(url)
		if err != nil {
			// Listener not open yet, or a transient dial error during
			// startup — keep polling.
			continue
		}
		var status httpapi.ReadinessStatus
		decodeErr := json.NewDecoder(response.Body).Decode(&status)
		_ = response.Body.Close()
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}

		if !status.Ready {
			sawNotReady = true
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("not-ready /readyz status = %d, want %d, body = %+v", response.StatusCode, http.StatusServiceUnavailable, status)
			}
			continue
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("ready /readyz status = %d, want %d, body = %+v", response.StatusCode, http.StatusOK, status)
		}
		final = status
		reachedReady = true
	}
	if !reachedReady {
		t.Fatal("daemon never reported ready within the deadline")
	}
	if !sawNotReady {
		t.Fatal("never observed /readyz report ready=false before the daemon became ready — a hardcoded Ready:true (#3806's literal regression) would pass this test undetected")
	}
	for _, check := range []string{"configLoaded", "stateOpen", "resumeComplete", "sweepsStarted"} {
		if !final.Checks[check] {
			t.Fatalf("readyz check %q = false once ready: checks = %+v", check, final.Checks)
		}
	}

	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("daemon exited: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for \"daemon started\"")
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
