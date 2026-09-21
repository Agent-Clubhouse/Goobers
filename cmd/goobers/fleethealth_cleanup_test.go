package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

type fleetCleanupWriter struct {
	readmodel.RetentionWriter
	fail atomic.Bool
}

func (w *fleetCleanupWriter) PruneChangeFeed(ctx context.Context, keep int) (int64, error) {
	if w.fail.Load() {
		return 0, errors.New("fixture cleanup failure")
	}
	return w.RetentionWriter.PruneChangeFeed(ctx, keep)
}

type fleetCleanupReader struct {
	*fleetTestReader
	service *readservice.Local
}

func (r *fleetCleanupReader) SchedulerStatus(ctx context.Context) (readservice.SchedulerStatus, error) {
	return r.service.SchedulerStatus(ctx)
}

// Only the prune boundary is fault-injected. State transitions, counters and
// completion timestamps come from RetentionLoop.Run and its real status mapper.
func TestFleetCleanupActualRetentionFailureAndRecovery(t *testing.T) {
	store, err := readmodel.Open(filepath.Join(t.TempDir(), readmodel.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	writer := &fleetCleanupWriter{RetentionWriter: store}
	loop := readmodel.NewRetentionLoop(store, writer, readmodel.UnboundedRetention(), readmodel.RetentionOptions{Interval: time.Hour})
	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:         instance.NewLayout(t.TempDir()),
		Config:         &instance.Config{},
		Definitions:    &instance.ConfigSet{Manifest: &apiv1.Manifest{}},
		RetentionStats: loop.Stats,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	reader := &fleetCleanupReader{fleetTestReader: &fleetTestReader{}, service: service}
	observer := &fleetHealthObserver{reader: reader, config: &instance.DiagnosticsConfig{}, instanceID: "deployment", bootID: "boot", startedAt: time.Now().Add(-time.Hour), eligibleSince: make(map[string]time.Time)}
	writer.fail.Store(true)
	failed := runFleetRetentionPass(t, loop, "failed")
	now := failed.LastCompletedAt.Add(time.Millisecond)
	reader.now = now
	records := observer.sample(context.Background(), now)
	if len(records) != 2 || records[0].Attributes["reasonCode"] != "cleanup_failure" || records[0].Attributes["state"] != "unknown" {
		t.Fatalf("actual failed cleanup absent: %+v", records)
	}
	if records[1].Attributes["reasonCode"] == "cleanup_failure" {
		t.Fatal("deployment cleanup attributed to unrelated gaggle")
	}
	for _, at := range []time.Time{failed.LastCompletedAt.Add(-time.Second), failed.LastCompletedAt.Add(2 * time.Minute)} {
		reader.now = at
		if got := observer.sample(context.Background(), at)[0].Attributes["reasonCode"]; got == "cleanup_failure" {
			t.Fatal("future or expired failure stayed active")
		}
	}
	writer.fail.Store(false)
	recovered := runFleetRetentionPass(t, loop, "completed")
	if recovered.Failures != 1 || recovered.Failed != 0 || recovered.LastResult != "completed" {
		t.Fatalf("expected cumulative failure plus successful latest pass: %+v", recovered)
	}
	now = recovered.LastCompletedAt.Add(time.Millisecond)
	reader.now = now
	if got := observer.sample(context.Background(), now)[0].Attributes["reasonCode"]; got == "cleanup_failure" {
		t.Fatal("successful retention pass did not clear failure")
	}
}

func runFleetRetentionPass(t *testing.T, loop *readmodel.RetentionLoop, want string) readmodel.RetentionStats {
	t.Helper()
	prior := loop.Stats().Passes
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); loop.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("retention loop did not stop")
		}
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		stats := loop.Stats()
		if stats.Passes > prior && stats.State == want && (want != "failed" || stats.Failed > 0) {
			return stats
		}
		select {
		case <-ctx.Done():
			t.Fatalf("retention pass did not settle: %+v", stats)
		case <-ticker.C:
		}
	}
}
