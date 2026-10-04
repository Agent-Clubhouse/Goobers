package daemonheartbeat

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readprobe"
)

func TestSummarizeHeartbeatCountsOnlyNewSchedulerActivity(t *testing.T) {
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventTriggerFired},
		{Seq: 3, Type: journal.EventTriggerFired},
		{Seq: 4, Type: journal.EventRunStarted},
		{Seq: 5, Type: journal.EventRunFinished},
		{Seq: 6, Type: journal.EventTickSkipped},
		{Seq: 7, Type: journal.EventClaimReleased},
	}

	got, lastSeq := summarizeHeartbeat(events, 2)
	want := heartbeatActivity{triggers: 1, started: 1, finished: 1, skipped: 1}
	if got != want {
		t.Fatalf("activity = %+v, want %+v", got, want)
	}
	if lastSeq != 7 {
		t.Fatalf("last seq = %d, want 7", lastSeq)
	}
}

func TestEmitHeartbeatsReadsConstantBytesPerTick(t *testing.T) {
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	for range 200 {
		if err := log.Append(journal.Event{Type: journal.EventTickSkipped, Reason: strings.Repeat("history", 20)}); err != nil {
			t.Fatal(err)
		}
	}
	tail, err := journal.OpenInstanceLogTail(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventTriggerFired, Workflow: "new"}); err != nil {
		t.Fatal(err)
	}

	readprobe.Enable()
	t.Cleanup(readprobe.Disable)
	ctx, cancel := context.WithCancel(context.Background())
	stdout := newDaemonOutput()
	done := make(chan struct{})
	go Emit(ctx, stdout, dir, func() int { return 1 }, tail, nil, 100*time.Millisecond, nil, done)

	select {
	case <-stdout.heartbeat:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("heartbeat was not emitted")
	}
	<-done

	work := readprobe.Take()
	if work.InstanceTailReads != 1 || work.InstanceTailBytes == 0 || work.InstanceTailBytes > 1024 {
		t.Fatalf("heartbeat work = %+v, want one read of at most 1024 bytes", work)
	}
	if output := stdout.String(); !strings.Contains(output, "1 trigger(s) fired") {
		t.Fatalf("heartbeat output = %q, want startup activity", output)
	}
}

// The memory clause is the whole reason #3949 was diagnosable only by hand: a
// heartbeat carrying scheduler counts alone cannot distinguish a leaking daemon
// from a memory cgroup filling with page cache from the stages it runs. The CPU
// clause answers the question that same incident could not (#3963) — a pod
// pinned at its CPU quota looks identical to a busy one in every point-in-time
// metric, and the throttling counters are the only term that separates them.
// Assert both are on the line, on the healthy and the degraded path alike.
func TestEmitHeartbeatsCarriesTheResourceFootprint(t *testing.T) {
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if err := log.Append(journal.Event{Type: journal.EventTriggerFired, Workflow: "w"}); err != nil {
		t.Fatal(err)
	}
	tail, err := journal.OpenInstanceLogTail(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		dir      string
		tail     *journal.InstanceLogTail
		wantLine string
	}{
		{name: "activity available", dir: dir, tail: tail, wantLine: "trigger(s) fired"},
		// A nil tail makes Emit reopen the instance log; pointing it
		// at a directory that has none drives the degraded branch.
		{name: "activity unavailable", dir: filepath.Join(t.TempDir(), "absent"), wantLine: "scheduler activity unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			stdout := newDaemonOutput()
			done := make(chan struct{})
			go Emit(ctx, stdout, tc.dir, func() int { return 1 }, tc.tail, nil, 10*time.Millisecond, nil, done)

			select {
			case <-stdout.heartbeat:
			case <-time.After(2 * time.Second):
				cancel()
				<-done
				t.Fatal("heartbeat was not emitted")
			}
			cancel()
			<-done

			output := stdout.String()
			for _, want := range []string{tc.wantLine, "heap ", "retained ", "goroutine(s)", "cpu ", "host", "GOMAXPROCS "} {
				if !strings.Contains(output, want) {
					t.Fatalf("heartbeat output = %q, want it to contain %q", output, want)
				}
			}
		})
	}
}

type daemonOutput struct {
	mu            sync.Mutex
	buf           bytes.Buffer
	heartbeat     chan struct{}
	heartbeatOnce sync.Once
}

func newDaemonOutput() *daemonOutput { return &daemonOutput{heartbeat: make(chan struct{})} }

func (o *daemonOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.buf.Write(p)
	if strings.Contains(o.buf.String(), "] alive — ") {
		o.heartbeatOnce.Do(func() { close(o.heartbeat) })
	}
	return n, err
}

func (o *daemonOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}
