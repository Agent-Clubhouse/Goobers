package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRunStartupPhaseLogsStartDoneAndFailure(t *testing.T) {
	var buf bytes.Buffer
	tracker := &startupPhaseTracker{}

	if err := runStartupPhase(&buf, tracker, "example-phase", "gaggle-a", func() error { return nil }); err != nil {
		t.Fatalf("runStartupPhase: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `phase=example-phase status=start target="gaggle-a"`) {
		t.Fatalf("output = %q, want start line", out)
	}
	if !strings.Contains(out, `phase=example-phase status=done target="gaggle-a"`) {
		t.Fatalf("output = %q, want done line", out)
	}
	if phase, target, since := tracker.snapshot(); phase != "" || target != "" || !since.IsZero() {
		t.Fatalf("tracker snapshot = (%q, %q, %s), want cleared completed phase", phase, target, since)
	}

	buf.Reset()
	failure := errors.New("boom: Authorization: Bearer sk-live-abcdef")
	err := runStartupPhase(&buf, tracker, "failing-phase", "gaggle-b", func() error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("runStartupPhase returned %v, want %v", err, failure)
	}
	out = buf.String()
	if !strings.Contains(out, `phase=failing-phase status=failed target="gaggle-b"`) {
		t.Fatalf("output = %q, want failed line", out)
	}
	if strings.Contains(out, "sk-live-abcdef") {
		t.Fatalf("output = %q, leaked secret from error", out)
	}
	if phase, target, since := tracker.snapshot(); phase != "" || target != "" || !since.IsZero() {
		t.Fatalf("tracker snapshot after failure = (%q, %q, %s), want cleared phase", phase, target, since)
	}
}

func TestRunStartupPhaseTransitionsFromCompletedPhaseToBlockedPhase(t *testing.T) {
	tracker := &startupPhaseTracker{}
	if err := runStartupPhase(io.Discard, tracker, "completed", "", func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	blocked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runStartupPhase(io.Discard, tracker, "blocked", "run-42", func() error {
			close(blocked)
			<-release
			return nil
		})
	}()
	<-blocked
	phase, target, since := tracker.snapshot()
	if phase != "blocked" || target != "run-42" || since.IsZero() {
		t.Fatalf("tracker snapshot = (%q, %q, %s), want active blocked phase", phase, target, since)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if phase, _, _ := tracker.snapshot(); phase != "" {
		t.Fatalf("completed blocked phase remained active as %q", phase)
	}
}

func TestWatchStartupReadinessEmitsDiagnosticNamingCurrentPhase(t *testing.T) {
	var buf syncBuffer
	tracker := &startupPhaseTracker{}
	tracker.set("stuck-phase", "big-repo")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		watchStartupReadiness(ctx, &buf, tracker, func() bool { return false }, 20*time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchStartupReadiness did not return")
	}

	out := buf.String()
	if !strings.Contains(out, "phase=stuck-phase") || !strings.Contains(out, `target="big-repo"`) {
		t.Fatalf("diagnostic output = %q, want it to name the stuck phase and target", out)
	}
}

func TestWatchStartupReadinessSilentOnceReady(t *testing.T) {
	var buf syncBuffer
	tracker := &startupPhaseTracker{}
	tracker.set("some-phase", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		watchStartupReadiness(ctx, &buf, tracker, func() bool { return true }, 20*time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchStartupReadiness did not return")
	}

	if out := buf.String(); out != "" {
		t.Fatalf("output = %q, want no diagnostic once ready", out)
	}
}
