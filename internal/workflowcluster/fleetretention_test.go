package workflowcluster

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newRetentionStore(t *testing.T, root string, policy FleetRetentionPolicy, now time.Time) *FileRetainedSnapshotStore {
	t.Helper()
	store, err := NewFileRetainedSnapshotStore(root, map[string]FleetRetentionPolicy{"fleet-1": policy})
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	return store
}

func TestRetainedSnapshotStoreRefusesWritesWhenDisabled(t *testing.T) {
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{}, fleetT0)
	err := store.Write(context.Background(), "fleet-1", snapshotOf(t, "test/v1", "inst-1", "fleet-1", fleetT0, blob("a", 0, 0, 2)))
	if !errors.Is(err, ErrFleetRetentionDisabled) {
		t.Fatalf("Write error = %v, want retention disabled", err)
	}
}

func TestRetainedSnapshotStorePrunesAgeAndPerInstanceCount(t *testing.T) {
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
		Enabled: true, MaxAge: 2 * time.Hour, MaxSnapshotsPerInstance: 2,
	}, fleetT0)
	for i, at := range []time.Time{
		fleetT0.Add(-3 * time.Hour), fleetT0.Add(-90 * time.Minute),
		fleetT0.Add(-time.Hour), fleetT0,
	} {
		snapshot := snapshotOf(t, "test/v1", "inst-1", "fleet-1", at, blob(string(rune('a'+i)), float64(i), 0, 2))
		if err := store.Write(context.Background(), "fleet-1", snapshot); err != nil {
			t.Fatal(err)
		}
	}
	snapshots, err := store.Snapshots(context.Background(), "fleet-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || !snapshots[0].GeneratedAt.Equal(fleetT0.Add(-time.Hour)) || !snapshots[1].GeneratedAt.Equal(fleetT0) {
		t.Fatalf("retained snapshots = %+v", snapshots)
	}
}

func TestRetainedSnapshotStorePurgesEverySnapshotForInstance(t *testing.T) {
	for _, event := range []string{"leave", "revoke", "scope-removal"} {
		t.Run(event, func(t *testing.T) {
			store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
				Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 10,
			}, fleetT0)
			for _, instance := range []string{"gone", "kept"} {
				for i := range 3 {
					snapshot := snapshotOf(t, "test/v1", instance, "fleet-1", fleetT0.Add(time.Duration(i)*time.Minute), blob(instance, float64(i), 0, 2))
					if err := store.Write(context.Background(), "fleet-1", snapshot); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.PurgeInstance(context.Background(), "fleet-1", "gone"); err != nil {
				t.Fatal(err)
			}
			snapshots, err := store.Snapshots(context.Background(), "fleet-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshots) != 3 {
				t.Fatalf("snapshots after %s = %+v", event, snapshots)
			}
			for _, snapshot := range snapshots {
				if snapshot.InstanceID != "kept" {
					t.Fatalf("snapshot for purged instance survived %s: %+v", event, snapshot)
				}
			}
		})
	}
}

func TestRetainedSnapshotFilesContainOnlyFleetSnapshotFields(t *testing.T) {
	root := t.TempDir()
	store := newRetentionStore(t, root, FleetRetentionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 2,
	}, fleetT0)
	features := extract(t, reviewLoop([4]string{"draft-zz", "inspect-zz", "decide-zz", "ship-zz"}, "secret goal text"))
	snapshot := snapshotOf(t, StructuralFeatureVersion, "inst-1", "fleet-1", fleetT0, []Point{
		StructuralPoint("secret-gaggle/secret-workflow", features),
	})
	if err := store.Write(context.Background(), "fleet-1", snapshot); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := filepath.WalkDir(root, func(candidate string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && filepath.Ext(candidate) == ".json" {
			path = candidate
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 6 {
		t.Fatalf("stored fields = %v", stored)
	}
	for _, leaked := range []string{"secret-gaggle", "secret-workflow", "secret goal", "-zz", "coder", "reviewer"} {
		if strings.Contains(string(data), leaked) {
			t.Fatalf("retained snapshot leaks %q: %s", leaked, data)
		}
	}
}

func TestFleetDriftReportsThreeWindowsOfOccupancyChange(t *testing.T) {
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 10,
	}, fleetT0.Add(3*time.Hour))
	write := func(instance string, at time.Time, points []Point) {
		t.Helper()
		if err := store.Write(context.Background(), "fleet-1", snapshotOf(t, "test/v1", instance, "fleet-1", at, points)); err != nil {
			t.Fatal(err)
		}
	}
	write("inst-1", fleetT0, blob("a", 0, 0, 4))
	write("inst-2", fleetT0, blob("b", 0, 0, 4))
	write("inst-1", fleetT0.Add(time.Hour), blob("c", 0, 0, 4))
	write("inst-2", fleetT0.Add(time.Hour), blob("d", 10, 0, 4))
	write("inst-1", fleetT0.Add(2*time.Hour), blob("e", 10, 0, 4))
	write("inst-2", fleetT0.Add(2*time.Hour), blob("f", 10, 0, 4))

	report, err := FleetDrift(context.Background(), store, "fleet-1", time.Hour, Options{
		MinClusterSize: 2, MinSamples: 2, Resamples: 10, Seed: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Windows) != 3 {
		t.Fatalf("windows = %+v", report.Windows)
	}
	for i, window := range report.Windows {
		if window.View.Workflows != 8 {
			t.Fatalf("window %d workflows = %d, want 8", i, window.View.Workflows)
		}
		sum := 0
		for _, occupancy := range window.Occupancy {
			sum += occupancy.Workflows
		}
		if sum != 8 {
			t.Fatalf("window %d occupancy = %+v", i, window.Occupancy)
		}
	}
	if report.Windows[0].Change != nil || report.Windows[1].Change == nil || report.Windows[2].Change == nil {
		t.Fatalf("changes = %+v", report.Windows)
	}
	const mixedDiversity = 1.5612781244591325
	if math.Abs(report.Windows[1].Change.OccupancyDistance-1) > 1e-9 ||
		math.Abs(report.Windows[1].Change.DiversityDelta-mixedDiversity) > 1e-9 ||
		math.Abs(report.Windows[2].Change.OccupancyDistance-1) > 1e-9 ||
		math.Abs(report.Windows[2].Change.DiversityDelta+mixedDiversity) > 1e-9 {
		t.Fatalf("unexpected occupancy changes: %+v", report.Windows)
	}
}
