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

func TestAzureReplayHealthGrowthLossRateLimitAndRecovery(t *testing.T) {
	now := time.Now()
	state := replayHealthState{}
	stats := AzureReplayStats{AccountingReady: true}
	if got := state.sample(now, stats, replayLossCounters{}, 10000); got != nil {
		t.Fatalf("healthy warning: %+v", got)
	}
	stats.PendingRecords = 10
	stats.Accepted = 10
	stats.OldestPendingAge = 10 * time.Second
	if got := state.sample(now.Add(10*time.Second), stats, replayLossCounters{}, 10000); got != nil {
		t.Fatalf("transient growth warning: %+v", got)
	}
	stats.PendingRecords = 20
	stats.Accepted = 20
	stats.OldestPendingAge = 20 * time.Second
	got := state.sample(now.Add(20*time.Second), stats, replayLossCounters{}, 10000)
	if got == nil || !slices.Contains(got.Causes, "ingress_exceeds_delivery") || got.AdmittedPerSecond != 1 || got.DeliveredPerSecond != 0 {
		t.Fatalf("growth warning=%+v", got)
	}
	stats.PendingRecords = 30
	stats.PrunedBytes = 1
	stats.OldestPendingAge = 30 * time.Second
	if got = state.sample(now.Add(30*time.Second), stats, replayLossCounters{Dropped: 7}, 10000); got != nil {
		t.Fatal("warning not suppressed")
	}
	stats.OldestPendingAge = 80 * time.Second
	got = state.sample(now.Add(80*time.Second), stats, replayLossCounters{Dropped: 7}, 10000)
	if got == nil || !slices.Contains(got.Causes, "loss_or_export_failure") || got.Queue.Dropped != 7 || got.PrunedBytes != 1 {
		t.Fatalf("loss not retained through suppression: %+v", got)
	}
	stats.PendingRecords = 0
	stats.Delivered = 30
	stats.OldestPendingAge = 0
	got = state.sample(now.Add(90*time.Second), stats, replayLossCounters{Dropped: 7}, 10000)
	if got == nil || got.Status != "recovered" {
		t.Fatalf("recovery=%+v", got)
	}
	if got = state.sample(now.Add(100*time.Second), stats, replayLossCounters{Dropped: 7}, 10000); got != nil {
		t.Fatal("recovery repeated")
	}
}

func TestAzureReplayHealthFreshBatchesAreNotSustainedBacklog(t *testing.T) {
	now := time.Now()
	state := replayHealthState{}
	// Reproduce the Windows accelerated run's sampling alias: counts rise at
	// successive samples, but all previously pending records have already left.
	// The oldest record at the observed warning was only 80.4855 ms old.
	for i, pending := range []int{0, 5, 11, 0} {
		stats := AzureReplayStats{
			AccountingReady:  true,
			PendingRecords:   pending,
			Accepted:         uint64(i * 200),
			Delivered:        uint64(i*200 - pending),
			OldestPendingAge: 80485500 * time.Nanosecond,
		}
		if pending == 0 {
			stats.OldestPendingAge = 0
		}
		if got := state.sample(now.Add(time.Duration(i)*replayHealthInterval), stats, replayLossCounters{}, 1<<20); got != nil {
			t.Fatalf("fresh batch produced a sustained-pressure warning at sample %d: %+v", i, got)
		}
	}
}

func TestAzureReplayHealthFreshBacklogKeepsIndependentWarnings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats AzureReplayStats
		loss  replayLossCounters
		cause string
	}{
		{"high_water", AzureReplayStats{AccountingReady: true, PendingBytes: 900}, replayLossCounters{}, "spool_high_water"},
		{"admission", AzureReplayStats{AccountingReady: true, AdmissionFailures: 1}, replayLossCounters{}, "loss_or_export_failure"},
		{"queue_loss", AzureReplayStats{AccountingReady: true}, replayLossCounters{Dropped: 1}, "loss_or_export_failure"},
		{"deferred", AzureReplayStats{AccountingReady: true}, replayLossCounters{CatchupDeferred: 1}, "journal_catchup_deferred"},
		{"accounting", AzureReplayStats{}, replayLossCounters{}, "accounting_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := replayHealthState{}
			stats := tc.stats
			stats.PendingRecords = 11
			stats.OldestPendingAge = time.Millisecond
			got := state.sample(time.Now(), stats, tc.loss, 1000)
			if got == nil || !slices.Contains(got.Causes, tc.cause) {
				t.Fatalf("independent %s warning suppressed: %+v", tc.cause, got)
			}
		})
	}
}

func TestAzureReplayHealthBacklogContinuityUsesActualSampleInterval(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		elapsed  time.Duration
		age      time.Duration
		ready    bool
		previous bool
		growing  int
	}{
		{"persistent_regular", 10 * time.Second, 10 * time.Second, true, true, 2},
		{"persistent_short", 5 * time.Second, 5 * time.Second, true, true, 2},
		{"persistent_delayed", 20 * time.Second, 20 * time.Second, true, true, 2},
		{"replaced_delayed", 20 * time.Second, 15 * time.Second, true, true, 0},
		{"fresh_batch", 10 * time.Second, time.Millisecond, true, true, 0},
		{"unknown_age", 10 * time.Second, 0, true, true, 0},
		{"stale_current", 10 * time.Second, 20 * time.Second, false, true, 0},
		{"stale_previous", 10 * time.Second, 20 * time.Second, true, false, 0},
		{"same_instant", 0, 20 * time.Second, true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := replayHealthState{
				at:       now,
				previous: AzureReplayStats{AccountingReady: tc.previous, PendingRecords: 5},
				growing:  1,
			}
			stats := AzureReplayStats{AccountingReady: tc.ready, PendingRecords: 11, OldestPendingAge: tc.age}
			got := state.sample(now.Add(tc.elapsed), stats, replayLossCounters{}, 1<<20)
			if state.growing != tc.growing {
				t.Fatalf("growth streak=%d, want %d; event=%+v", state.growing, tc.growing, got)
			}
			warned := got != nil && slices.Contains(got.Causes, "ingress_exceeds_delivery")
			if warned != (tc.growing >= 2) {
				t.Fatalf("sustained-growth warning=%t, event=%+v", warned, got)
			}
		})
	}
}

func TestAzureReplayHealthOldBacklogHighWaterAndUnavailableAccounting(t *testing.T) {
	state := replayHealthState{}
	got := state.sample(time.Now(), AzureReplayStats{PendingBytes: 900, OldestPendingAge: time.Minute}, replayLossCounters{}, 1000)
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
