package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAzureReplayHealthRemoteCountersMatchLocalLossSource(t *testing.T) {
	root := t.TempDir()
	s, err := newAzureReplaySpool(azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20}, func(context.Context, []byte) error { return errors.New("offline") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.close(ctx)
	})
	setReplayLossSource(&s.lossSource, func() replayLossCounters { return replayLossCounters{Dropped: 7, ExportFailures: 2} })
	if err = s.submit(context.Background(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	for _, stats := range []AzureReplayStats{s.stats(), InspectAzureReplayRoot(root)} {
		if !stats.AccountingReady || stats.PendingFiles != 1 || stats.QueueDropped != 7 || stats.ExportFailures != 2 {
			t.Fatalf("loss visibility mismatch: %+v", stats)
		}
	}
}

func TestAzureReplayShutdownEmitsFinalCounterSnapshot(t *testing.T) {
	root := t.TempDir()
	s, err := newAzureReplaySpool(azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20},
		func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.index.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	setReplayLossSource(&s.lossSource, func() replayLossCounters {
		return replayLossCounters{Dropped: 2, ExportFailures: 1, CatchupDeferred: 3}
	})
	if err := s.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "health-journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var final replayHealthEvent
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var event replayHealthEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Status == "shutdown" {
			final = event
		}
	}
	if final.Event != "telemetry.export.health" || final.Stream != "journal" || final.Queue.Dropped != 2 ||
		final.Queue.ExportFailures != 1 || final.Queue.CatchupDeferred != 3 || final.AdmissionFailures != 0 {
		t.Fatalf("incomplete final health snapshot: %+v", final)
	}
}

func TestAzureReplayHealthGrowthLossRateLimitAndRecovery(t *testing.T) {
	now := time.Now()
	state := replayHealthState{}
	stats := AzureReplayStats{AccountingReady: true}
	if got := state.sample(now, stats, replayLossCounters{}, 10000); got != nil {
		t.Fatalf("healthy warning: %+v", got)
	}
	stats.PendingRecords = 10
	stats.Accepted = 10
	if got := state.sample(now.Add(10*time.Second), stats, replayLossCounters{}, 10000); got != nil {
		t.Fatalf("transient growth warning: %+v", got)
	}
	stats.PendingRecords = 20
	stats.Accepted = 20
	got := state.sample(now.Add(20*time.Second), stats, replayLossCounters{}, 10000)
	if got == nil || !slices.Contains(got.Causes, "ingress_exceeds_delivery") || got.AdmittedPerSecond != 1 || got.DeliveredPerSecond != 0 {
		t.Fatalf("growth warning=%+v", got)
	}
	stats.PendingRecords = 30
	stats.PrunedBytes = 1
	if got = state.sample(now.Add(30*time.Second), stats, replayLossCounters{Dropped: 7}, 10000); got != nil {
		t.Fatal("warning not suppressed")
	}
	got = state.sample(now.Add(80*time.Second), stats, replayLossCounters{Dropped: 7}, 10000)
	if got == nil || !slices.Contains(got.Causes, "loss_or_export_failure") || got.Queue.Dropped != 7 || got.PrunedBytes != 1 {
		t.Fatalf("loss not retained through suppression: %+v", got)
	}
	stats.PendingRecords = 0
	stats.Delivered = 30
	got = state.sample(now.Add(90*time.Second), stats, replayLossCounters{Dropped: 7}, 10000)
	if got == nil || got.Status != "recovered" {
		t.Fatalf("recovery=%+v", got)
	}
	if got = state.sample(now.Add(100*time.Second), stats, replayLossCounters{Dropped: 7}, 10000); got != nil {
		t.Fatal("recovery repeated")
	}
}

func TestAzureReplayHealthOldBacklogHighWaterAndUnavailableAccounting(t *testing.T) {
	state := replayHealthState{}
	got := state.sample(time.Now(), AzureReplayStats{PendingBytes: 900, OldestPendingAge: time.Minute}, replayLossCounters{}, 1000)
	if got == nil || got.AccountingReady {
		t.Fatalf("partial backlog was reported as fully accounted: %+v", got)
	}
	for _, cause := range []string{"accounting_unavailable", "spool_high_water", "backlog_old"} {
		if got == nil || !slices.Contains(got.Causes, cause) {
			t.Fatalf("missing %s: %+v", cause, got)
		}
	}
}

func TestAzureReplayHealthBusyAccountingKeepsBacklogAndRecoversAfterDelivery(t *testing.T) {
	now := time.Now()
	s := testAzureReplaySpool(t, t.TempDir(), now)
	const payload = "{\"id\":\"busy-accounting\"}\n"
	if err := s.submit(t.Context(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	before := s.stats()
	if !before.AccountingReady || before.PendingRecords != 1 || before.PendingFiles != 1 || before.PendingBytes == 0 {
		t.Fatalf("initial accounting: %+v", before)
	}
	state := replayHealthState{}
	if event := state.sample(now, before, replayLossCounters{}, s.cfg.maxBytes); event != nil {
		t.Fatalf("unexpected initial warning: %+v", event)
	}

	// Hold the health reader's sole connection to deterministically exhaust its
	// own budget. Writer contention is covered separately and must not do this.
	conn, err := s.index.statsDB.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	_, err = s.index.stats(ctx, s.stream, now)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy connection should exhaust the query budget: %v", err)
	}
	done := make(chan AzureReplayStats, 1)
	go func() { done <- s.stats() }()
	var busy AzureReplayStats
	select {
	case busy = <-done:
	case <-time.After(2 * time.Second):
		_ = conn.Close()
		t.Fatal("health snapshot waited for the busy reader instead of its bounded deadline")
	}
	if busy.AccountingReady || busy.PendingRecords != before.PendingRecords || busy.PendingFiles != before.PendingFiles || busy.PendingBytes != before.PendingBytes {
		t.Fatalf("unavailable accounting discarded the last known backlog: %+v", busy)
	}
	if busy.AdmissionFailures != 0 || busy.ExportFailures != 0 || busy.QueueDropped != 0 || busy.PrunedAge != 0 || busy.PrunedBytes != 0 || busy.Malformed != 0 {
		t.Fatalf("health query timeout was incorrectly counted as record loss: %+v", busy)
	}
	event := state.sample(now.Add(10*time.Second), busy, replayLossCounters{}, s.cfg.maxBytes)
	if event == nil || event.Status != "warning" || !slices.Equal(event.Causes, []string{"accounting_unavailable"}) {
		t.Fatalf("unavailable accounting must remain visible: %+v", event)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	ready := s.stats()
	if !ready.AccountingReady || ready.PendingRecords != 1 {
		t.Fatalf("accounting did not become available after reader released: %+v", ready)
	}
	if event := state.sample(now.Add(20*time.Second), ready, replayLossCounters{}, s.cfg.maxBytes); event != nil && event.Status == "recovered" {
		t.Fatalf("fresh accounting alone falsely proved delivery recovery: %+v", event)
	}
	if !state.warning {
		t.Fatal("warning cleared before subsequent delivery")
	}
	s.send = func(_ context.Context, body []byte) error {
		if string(body) != payload {
			t.Errorf("retained replay payload changed: %q", body)
		}
		return nil
	}
	if err := s.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := s.stats()
	if !after.AccountingReady || after.PendingRecords != 0 || after.PendingFiles != 0 || after.Delivered != 1 || after.AdmissionFailures != 0 || after.ExportFailures != 0 {
		t.Fatalf("delivery after accounting recovery: %+v", after)
	}
	event = state.sample(now.Add(30*time.Second), after, replayLossCounters{}, s.cfg.maxBytes)
	if event == nil || event.Status != "recovered" {
		t.Fatalf("successful subsequent delivery did not clear accounting warning: %+v", event)
	}
}

func TestAzureReplayHealthIdleStreamDoesNotProveRecovery(t *testing.T) {
	now := time.Now()
	state := replayHealthState{}
	stats := AzureReplayStats{AccountingReady: true, AdmissionFailures: 1}
	if got := state.sample(now, stats, replayLossCounters{}, 1000); got == nil || got.Status != "warning" {
		t.Fatalf("missing initial failure: %+v", got)
	}
	// A quiet stream has not tested either storage admission or remote delivery.
	if got := state.sample(now.Add(10*time.Second), stats, replayLossCounters{}, 1000); got != nil {
		t.Fatalf("idle stream falsely recovered: %+v", got)
	}
	got := state.sample(now.Add(time.Minute), stats, replayLossCounters{}, 1000)
	if got == nil || got.Status != "warning" || !slices.Contains(got.Causes, "recovery_unconfirmed") {
		t.Fatalf("unconfirmed recovery must remain visible: %+v", got)
	}
	// A delivery concurrent with another failure is not evidence that the
	// failure has cleared; require subsequent delivery with no active causes.
	stats.Delivered = 1
	stats.AdmissionFailures++
	state.sample(now.Add(70*time.Second), stats, replayLossCounters{}, 1000)
	state.sample(now.Add(120*time.Second), stats, replayLossCounters{}, 1000)
	if got = state.sample(now.Add(130*time.Second), stats, replayLossCounters{}, 1000); got != nil {
		t.Fatalf("delivery before latest failure falsely recovered: %+v", got)
	}
	stats.Delivered++
	got = state.sample(now.Add(140*time.Second), stats, replayLossCounters{}, 1000)
	if got == nil || got.Status != "recovered" {
		t.Fatalf("subsequent successful delivery did not recover: %+v", got)
	}
}

func TestAzureReplayHealthFileIsPrivateBoundedAndIndependent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "health-journal.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", replayHealthFileLimit)), 0o600); err != nil {
		t.Fatal(err)
	}
	event := replayHealthEvent{Event: "telemetry.export.health", Status: "warning", Stream: "journal", Queue: replayLossCounters{Dropped: 12}}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err = appendReplayHealth(root, "journal", append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + ".1"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > replayHealthFileLimit || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
			t.Fatalf("unbounded/insecure health file: %v", info)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
			t.Fatal("health log entered replay queue")
		}
	}
	if err = appendReplayHealth(root, "../escape", data); err == nil {
		t.Fatal("invalid stream accepted")
	}
}
