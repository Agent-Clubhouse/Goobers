package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// #5075: the heartbeat took the workflow count as an int captured at daemon
// startup, so a successful hot reload that added workflows kept printing the
// old number and made a healthy reload read as incomplete. It now polls a live
// accessor, so the printed count follows the reload.
func TestEmitHeartbeatsPrintsLiveWorkflowCount(t *testing.T) {
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if err := log.Append(journal.Event{Type: journal.EventTriggerFired, Workflow: "one"}); err != nil {
		t.Fatal(err)
	}
	tail, err := journal.OpenInstanceLogTail(dir)
	if err != nil {
		t.Fatal(err)
	}

	var count atomic.Int64
	count.Store(3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := newDaemonOutput()
	done := make(chan struct{})
	go emitHeartbeats(ctx, stdout, dir, func() int { return int(count.Load()) }, tail, nil, 10*time.Millisecond, nil, done)

	waitForHeartbeatCount(t, stdout, "3 workflow(s)")
	// Simulate the config reload applying two more workflows.
	count.Store(5)
	waitForHeartbeatCount(t, stdout, "5 workflow(s)")

	cancel()
	<-done
}

func waitForHeartbeatCount(t *testing.T, out *daemonOutput, want string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			out.mu.Lock()
			got := out.buf.String()
			out.mu.Unlock()
			t.Fatalf("heartbeat never reported %q; output:\n%s", want, got)
		case <-time.After(10 * time.Millisecond):
			out.mu.Lock()
			got := out.buf.String()
			out.mu.Unlock()
			if strings.Contains(got, want) {
				return
			}
		}
	}
}
