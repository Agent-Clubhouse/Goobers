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
	for _, cause := range []string{"accounting_unavailable", "spool_high_water", "backlog_old"} {
		if got == nil || !slices.Contains(got.Causes, cause) {
			t.Fatalf("missing %s: %+v", cause, got)
		}
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
