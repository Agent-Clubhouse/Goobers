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
