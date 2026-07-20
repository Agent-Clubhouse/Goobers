//go:build unix

package runner

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestFinishTaskDispatchRepairsPartialHeartbeatBeforeSideEffects(t *testing.T) {
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{
		RunID:    "0af7651916cd43dd8448eb211c80319c",
		Workflow: "implementation",
		Gaggle:   "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })

	eventsPath := filepath.Join(run.Dir(), "events.jsonl")
	info, err := os.Stat(eventsPath)
	if err != nil {
		t.Fatalf("stat events log: %v", err)
	}

	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatalf("get file-size limit: %v", err)
	}
	limited := original
	limited.Cur = uint64(info.Size() + 16)
	if limited.Cur >= original.Cur {
		t.Skip("file-size limit is too low for partial-write injection")
	}
	signal.Ignore(syscall.SIGXFSZ)
	restored := false
	restoreLimit := func() {
		if restored {
			return
		}
		restored = true
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
			t.Errorf("restore file-size limit: %v", err)
		}
		signal.Reset(syscall.SIGXFSZ)
	}
	t.Cleanup(restoreLimit)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatalf("set file-size limit: %v", err)
	}

	ticker := &fakeHeartbeatTicker{
		ticks:   make(chan time.Time, 1),
		stopped: make(chan struct{}),
	}
	r := &Runner{
		heartbeatInterval: StageHeartbeatInterval,
		newHeartbeatTicker: func(time.Duration) heartbeatTicker {
			return ticker
		},
	}
	heartbeat := r.startStageHeartbeat(run, "implement", 2, journal.AttemptPolicy)
	ticker.ticks <- time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC)
	select {
	case <-ticker.stopped:
	case <-time.After(time.Second):
		t.Fatal("heartbeat append did not hit the file-size limit")
	}
	restoreLimit()

	err = finishTaskDispatch(run, heartbeat, "implement", 2, journal.AttemptPolicy, []mutationFact{{
		Provider:  "github",
		Kind:      "pr",
		ID:        "1041",
		URL:       "https://github.com/acme/web/pull/1041",
		Operation: "open",
	}}, errors.New("remove worktree"))
	if err == nil {
		t.Fatal("finishTaskDispatch succeeded after heartbeat append failure")
	}
	if !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("finishTaskDispatch error = %v, want file-size limit failure", err)
	}

	reader, err := journal.OpenRead(run.Dir())
	if err != nil {
		t.Fatalf("open repaired journal: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("read repaired journal: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %+v, want run.started, repaired, ref.touched, and teardown error", events)
	}
	for i, event := range events {
		if event.Seq != uint64(i+1) {
			t.Fatalf("event %d sequence = %d, want %d", i, event.Seq, i+1)
		}
	}
	if events[1].Type != journal.EventRepaired {
		t.Fatalf("event after torn heartbeat = %+v, want repaired", events[1])
	}
	refEvent := events[2]
	if refEvent.Type != journal.EventRefTouched ||
		refEvent.Stage != "implement" ||
		refEvent.Attempt != 2 ||
		refEvent.AttemptClass != journal.AttemptPolicy ||
		refEvent.ExternalRef == nil ||
		refEvent.ExternalRef.ID != "1041" ||
		refEvent.Runner["operation"] != "open" {
		t.Fatalf("ref.touched event = %+v", refEvent)
	}
	removeEvent := events[3]
	if removeEvent.Type != journal.EventError ||
		removeEvent.Error == nil ||
		removeEvent.Error.Code != "worktree_remove_failed" {
		t.Fatalf("worktree removal event = %+v", removeEvent)
	}
}
