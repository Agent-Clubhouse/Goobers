package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageMeasurementsTrackCreateAndTeardown(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	var measurements []UsageMeasurement
	m, err := NewManager(t.TempDir(), WithUsageObserver("alpha", func(_ context.Context, measurement UsageMeasurement) {
		measurements = append(measurements, measurement)
	}))
	if err != nil {
		t.Fatal(err)
	}

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-stage", OwnerRunID: "run", BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(measurements) != 1 {
		t.Fatalf("create measurements = %d, want 1", len(measurements))
	}
	created := measurements[0]
	if created.Operation != UsageOperationCreate || created.Gaggle != "alpha" ||
		created.OwnerRunID != "run" || created.WorktreeID != wt.RunID {
		t.Fatalf("create measurement identity = %+v", created)
	}
	if !created.WorktreeMeasured || !created.WorkcopyMeasured || created.WorktreeBytes <= 0 ||
		created.WorkcopyBytes < created.WorktreeBytes || created.UnmeasuredWorktrees != 0 || created.Err != nil {
		t.Fatalf("create measurement = %+v", created)
	}

	payload := strings.Repeat("x", 4096)
	if err := os.WriteFile(filepath.Join(wt.Path, "generated.txt"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(measurements) != 2 {
		t.Fatalf("lifecycle measurements = %d, want 2", len(measurements))
	}
	removed := measurements[1]
	if removed.Operation != UsageOperationTeardown || removed.OwnerRunID != "run" ||
		removed.WorktreeID != wt.RunID || removed.WorktreeBytes < created.WorktreeBytes+int64(len(payload)) {
		t.Fatalf("teardown measurement = %+v, create = %+v", removed, created)
	}
	if !removed.WorkcopyMeasured || removed.WorkcopyBytes >= created.WorkcopyBytes || removed.Err != nil {
		t.Fatalf("aggregate did not shrink after teardown: create=%+v teardown=%+v", created, removed)
	}
}

func TestUsageMeasurementsTrackHousekeepingWithoutScanningLiveWorktrees(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	var measurements []UsageMeasurement
	m, err := NewManager(t.TempDir(), WithUsageObserver("alpha", func(_ context.Context, measurement UsageMeasurement) {
		measurements = append(measurements, measurement)
	}))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "orphan-stage", OwnerRunID: "orphan", BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	measurements = nil

	mk, err := readMarker(m.markerPath(wt.key, wt.RunID))
	if err != nil {
		t.Fatal(err)
	}
	mk.PID = 999999
	if err := writeMarker(m.markerPath(wt.key, wt.RunID), mk); err != nil {
		t.Fatal(err)
	}
	previous := processAlive
	processAlive = func(pid int) bool { return pid != 999999 }
	t.Cleanup(func() { processAlive = previous })

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil || len(warnings) != 0 || len(results) != 1 {
		t.Fatalf("Reap = results %+v, warnings %+v, err %v", results, warnings, err)
	}
	if len(measurements) != 2 {
		t.Fatalf("housekeeping measurements = %d, want per-worktree and aggregate: %+v", len(measurements), measurements)
	}
	reaped := measurements[0]
	if reaped.Operation != UsageOperationHousekeeping || reaped.OwnerRunID != "orphan" ||
		reaped.WorktreeID != wt.RunID || !reaped.WorktreeMeasured || !reaped.WorkcopyMeasured || reaped.Err != nil {
		t.Fatalf("reaped measurement = %+v", reaped)
	}
	summary := measurements[1]
	if summary.WorktreeID != "" || summary.Operation != UsageOperationHousekeeping ||
		!summary.WorkcopyMeasured || summary.UnmeasuredWorktrees != 0 || summary.Err != nil {
		t.Fatalf("housekeeping summary = %+v", summary)
	}
}

func TestUsageMeasurementsTrackRetentionDeletionThroughSharedReaper(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	var measurements []UsageMeasurement
	m, err := NewManager(t.TempDir(), WithUsageObserver("alpha", func(_ context.Context, measurement UsageMeasurement) {
		measurements = append(measurements, measurement)
	}))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "retained-stage", OwnerRunID: "retained", BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, RemoveOptions{Keep: true}); err != nil {
		t.Fatal(err)
	}
	retained := measurements[len(measurements)-1]
	measurements = nil

	markerPath := m.markerPath(wt.key, wt.RunID)
	mk, err := readMarker(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reapOne(ctx, wt.key, wt.Path, markerPath, &mk); err != nil {
		t.Fatal(err)
	}

	if len(measurements) != 1 {
		t.Fatalf("retention measurements = %d, want 1: %+v", len(measurements), measurements)
	}
	got := measurements[0]
	if got.Operation != UsageOperationHousekeeping || got.OwnerRunID != "retained" ||
		got.WorktreeID != wt.RunID || !got.WorktreeMeasured || !got.WorkcopyMeasured || got.Err != nil {
		t.Fatalf("retention measurement = %+v", got)
	}
	if got.WorkcopyBytes >= retained.WorkcopyBytes || got.UnmeasuredWorktrees != 0 {
		t.Fatalf("retention aggregate did not shrink: retained=%+v reaped=%+v", retained, got)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("retained worktree survived shared reaper: %v", err)
	}
}

func TestAggregateUsageUsesSnapshotInsteadOfScanningActiveWorktree(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	var measurements []UsageMeasurement
	m, err := NewManager(t.TempDir(), WithUsageObserver("alpha", func(_ context.Context, measurement UsageMeasurement) {
		measurements = append(measurements, measurement)
	}))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "live-stage", OwnerRunID: "live", BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline := measurements[0].WorkcopyBytes
	if err := os.WriteFile(filepath.Join(wt.Path, "large-active-file"), []byte(strings.Repeat("x", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	measurements = nil

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil || len(results) != 0 || len(warnings) != 0 {
		t.Fatalf("Reap live worktree = results %+v, warnings %+v, err %v", results, warnings, err)
	}
	if len(measurements) != 1 {
		t.Fatalf("housekeeping measurements = %+v, want one aggregate", measurements)
	}
	if got := measurements[0]; !got.WorkcopyMeasured || got.WorkcopyBytes != baseline ||
		got.UnmeasuredWorktrees != 0 || got.Err != nil {
		t.Fatalf("active worktree changed safe aggregate: baseline=%d measurement=%+v", baseline, got)
	}
}

func TestUsageMeasurementFailureIsObservedWithoutFailingTeardown(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	var measurements []UsageMeasurement
	m, err := NewManager(t.TempDir(), WithUsageObserver("alpha", func(_ context.Context, measurement UsageMeasurement) {
		measurements = append(measurements, measurement)
	}))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-stage", OwnerRunID: "run", BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	measurements = nil
	realDiskUsage := m.diskUsage
	m.diskUsage = func(path string) (int64, error) {
		if filepath.Clean(path) == filepath.Clean(wt.Path) {
			return 0, errors.New("fixture measurement failure")
		}
		return realDiskUsage(path)
	}

	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("measurement failure changed teardown outcome: %v", err)
	}
	if len(measurements) != 1 {
		t.Fatalf("teardown measurements = %d, want 1", len(measurements))
	}
	got := measurements[0]
	if got.Err == nil || got.WorktreeMeasured || !got.WorkcopyMeasured ||
		!strings.Contains(got.Err.Error(), "fixture measurement failure") {
		t.Fatalf("measurement failure was not surfaced: %+v", got)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree survived teardown: %v", err)
	}
}
