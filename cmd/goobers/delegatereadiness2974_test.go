package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// TestClientWaitOutlivesTheRequestDeadline is #2974's core ordering property.
//
// The client's wait and the request's lifetime used to be the same number, and
// that is what produced a failure with no answer in it: on a Windows daemon
// restart the client waited its 30 seconds and reported "timed out ... is the
// daemon still running and healthy?", then the daemon swept about six seconds
// later and — correctly, under #537 — refused the request as stale. The daemon
// was healthy and the request was definitively resolved; the operator was told
// neither.
//
// The windows are now ordered rather than shared, so the daemon always has
// room to publish its verdict before the client stops listening.
func TestClientWaitOutlivesTheRequestDeadline(t *testing.T) {
	if triggerResponseWait() <= triggerDelegationTimeout {
		t.Fatalf("client wait %s does not outlive the request deadline %s; the two share a boundary "+
			"again and a definitive daemon verdict can be missed", triggerResponseWait(), triggerDelegationTimeout)
	}
	if triggerResponseGrace <= 0 {
		t.Fatalf("grace = %s, want a positive margin covering a sweep interval and its write", triggerResponseGrace)
	}
}

// TestSlowSweepDeliversTheDaemonVerdictInsteadOfATimeout is the reported
// scenario end to end: the daemon sweeps LATE — after the request's own
// deadline has passed — and the client must come away with the daemon's stale
// refusal rather than a bare timeout that blames daemon health.
//
// The safety property is unchanged and is asserted here too: the late sweep
// still refuses to dispatch, so a request the operator was told about can
// never run afterwards (#537).
func TestSlowSweepDeliversTheDaemonVerdictInsteadOfATimeout(t *testing.T) {
	l := layoutFor(initDemo(t))
	schedulerDir := l.SchedulerDir()
	if err := os.MkdirAll(filepath.Join(schedulerDir, pendingTriggersDir), 0o755); err != nil {
		t.Fatal(err)
	}

	requestID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "acme-web", "implementation")
	if err != nil {
		t.Fatalf("writeTriggerRequestContext: %v", err)
	}

	// The daemon sweeps after the request expired but while the client is
	// still listening — the window the grace exists to create. Stand in for
	// the sweep by publishing the same stale refusal it writes.
	staleRefusal := "delegate: stale trigger request " + requestID + " reached its deadline; refusing to dispatch"
	respPath := filepath.Join(schedulerDir, pendingTriggersDir, requestID+responseSuffix)
	payload, err := json.Marshal(triggerResponse{Error: staleRefusal})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteFileAtomic(respPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = pollTriggerResponse(context.Background(), schedulerDir, requestID, triggerResponseWait())
	if err == nil {
		t.Fatal("poll err = nil, want the daemon's refusal surfaced")
	}
	if !strings.Contains(err.Error(), "stale trigger request") {
		t.Fatalf("err = %v, want the daemon's own verdict rather than a client-side timeout", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want no timeout: the daemon answered inside the client's wait", err)
	}
}

// TestDelegationTimeoutNamesWhichDaemonStateItFound is the diagnostic half.
//
// Reaching a timeout now means the daemon never answered at all, which leaves
// exactly two explanations — a scheduler that is up but not yet sweeping, and
// no live daemon — and they call for opposite responses. The heartbeat
// distinguishes them, so the message says which rather than asking.
func TestDelegationTimeoutNamesWhichDaemonStateItFound(t *testing.T) {
	t.Run("scheduler ticking recently", func(t *testing.T) {
		schedulerDir := t.TempDir()
		writeHeartbeat(t, schedulerDir, time.Now())

		got := schedulerLivenessEvidence(schedulerDir)
		if !strings.Contains(got, "a daemon is live") || !strings.Contains(got, "retry") {
			t.Fatalf("evidence = %q, want it to report a live daemon whose sweep has not caught up", got)
		}
	})

	t.Run("scheduler long silent", func(t *testing.T) {
		schedulerDir := t.TempDir()
		writeHeartbeat(t, schedulerDir, time.Now().Add(-time.Hour))

		got := schedulerLivenessEvidence(schedulerDir)
		if !strings.Contains(got, "no live daemon") {
			t.Fatalf("evidence = %q, want it to report no live daemon", got)
		}
	})

	t.Run("heartbeat unreadable", func(t *testing.T) {
		got := schedulerLivenessEvidence(t.TempDir())
		if !strings.Contains(got, "unknown") {
			t.Fatalf("evidence = %q, want it to say the state is unknown rather than assert either one", got)
		}
	})
}

func writeHeartbeat(t *testing.T, schedulerDir string, at time.Time) {
	t.Helper()
	lockPath := filepath.Join(schedulerDir, "up.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lockPath, at, at); err != nil {
		t.Fatal(err)
	}
}
