package main

import (
	"errors"
	"testing"
	"time"
)

func TestHarnessPreflightCompletionMessageReportsDuration(t *testing.T) {
	elapsed := 29200 * time.Millisecond
	if got, want := harnessPreflightCompletionMessage(nil, elapsed), "agentic harnesses ready (preflight duration 29.2s)"; got != want {
		t.Fatalf("success message = %q, want %q", got, want)
	}
	if got, want := harnessPreflightCompletionMessage(errors.New("signed out"), elapsed), "agentic harness preflight failed (duration 29.2s)"; got != want {
		t.Fatalf("failure message = %q, want %q", got, want)
	}
}
