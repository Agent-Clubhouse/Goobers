package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuidedInitBrowserNoOpenPrintsURLAndStopsCleanly(t *testing.T) {
	workdir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := &dashboardURLWriter{url: make(chan string, 1)}
	done := make(chan int, 1)
	originalLauncher := launchDashboardBrowser
	browserCalled := false
	launchDashboardBrowser = func(context.Context, string) error {
		browserCalled = true
		return nil
	}
	defer func() { launchDashboardBrowser = originalLauncher }()

	args := dashboardTestArgs(t, "--no-open", "--workdir", workdir)
	go func() {
		done <- runGuidedInitBrowserContext(ctx, args, started, io.Discard)
	}()

	var address string
	select {
	case address = <-started.url:
	case code := <-done:
		t.Fatalf("guided init exited before startup: code = %d", code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for getting-started URL")
	}
	if browserCalled {
		t.Fatal("--no-open launched a browser")
	}
	if !strings.HasSuffix(address, "/#/getting-started") || !strings.HasPrefix(address, "http://127.0.0.1:") {
		t.Fatalf("guided init URL = %q", address)
	}

	base := strings.TrimSuffix(address, "#/getting-started")
	response, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read index: %v %v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `content="getting-started"`) {
		t.Fatalf("portal response = %d %q", response.StatusCode, body)
	}

	// No tutorial instance exists yet, so the read API reports that explicitly.
	api, err := http.Get(base + "api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	apiBody, readErr := io.ReadAll(api.Body)
	closeErr = api.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read api response: %v %v", readErr, closeErr)
	}
	if api.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(apiBody), "guided_no_instance") {
		t.Fatalf("pre-instance API response = %d %q", api.StatusCode, apiBody)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("guided init exit code = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guided init did not stop after cancellation")
	}
}

func TestGuidedInitBrowserUsageErrors(t *testing.T) {
	if code := runGuidedInitBrowserContext(context.Background(), []string{"unexpected-path"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("positional arg exit code = %d, want 2", code)
	}
	if code := runGuidedInitBrowserContext(context.Background(), []string{"--port=notaport"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("bad port exit code = %d, want 2", code)
	}
	notDirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDirectory, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runGuidedInitBrowserContext(context.Background(), []string{"--workdir", notDirectory}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("file workdir exit code = %d, want 2", code)
	}
}

func TestGuidedInitBrowserUsesExplicitInstancePath(t *testing.T) {
	workdir := t.TempDir()
	instancePath := filepath.Join(t.TempDir(), "durable-instance")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := &dashboardURLWriter{url: make(chan string, 1)}
	done := make(chan int, 1)

	args := dashboardTestArgs(t,
		"--no-open", "--workdir", workdir, "--instance-path", instancePath,
	)
	go func() {
		done <- runGuidedInitBrowserContext(ctx, args, started, io.Discard)
	}()

	var address string
	select {
	case address = <-started.url:
	case code := <-done:
		t.Fatalf("guided init exited before startup: code = %d", code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for getting-started URL")
	}
	base := strings.TrimSuffix(address, "#/getting-started")
	response, err := http.Get(base + "guided/state")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var state guidedStateBody
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.InstancePath != instancePath {
		t.Fatalf("guided instance path = %q, want %q", state.InstancePath, instancePath)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("guided init exit code = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guided init did not stop after cancellation")
	}
}
