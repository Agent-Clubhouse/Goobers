package workflowcluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClusterFleetOnDemandReportsEveryFetchOutcome(t *testing.T) {
	valid := map[string]FleetSnapshot{
		"ok-1": snapshotOf(t, "test/v1", "ok-1", "s", fleetT0, blob("a", 0, 0, 4)),
		"ok-2": snapshotOf(t, "test/v1", "ok-2", "s", fleetT0, blob("b", 10, 0, 4)),
		"skew": snapshotOf(t, "test/v2", "skew", "s", fleetT0, blob("c", 20, 0, 4)),
	}
	tampered := snapshotOf(t, "test/v1", "tampered", "s", fleetT0, blob("d", 30, 0, 4))
	tampered.Workflows[0].Numeric["x"] = 99
	valid["tampered"] = tampered

	fetch := func(ctx context.Context, target FleetTarget) (FleetSnapshot, error) {
		switch target.InstanceID {
		case "offline":
			return FleetSnapshot{}, ErrFleetTargetOffline
		case "denied":
			return FleetSnapshot{}, ErrFleetProjectionNotGranted
		case "slow":
			<-ctx.Done()
			return FleetSnapshot{}, ctx.Err()
		default:
			return valid[target.InstanceID], nil
		}
	}
	targets := fleetTargets("ok-1", "offline", "denied", "slow", "tampered", "ok-2", "skew")
	view, err := ClusterFleetOnDemand(context.Background(), targets, fetch, FleetCollectionOptions{
		MaxConcurrency: 3, PerInstanceTimeout: 10 * time.Millisecond, OverallTimeout: time.Second,
	}, Options{MinClusterSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := fleetInstanceIDs(view.Instances); !equalStrings(got, []string{"ok-1", "ok-2"}) {
		t.Fatalf("included instances = %v", got)
	}
	reasons := map[string]string{}
	for _, exclusion := range view.Excluded {
		reasons[exclusion.InstanceID] = exclusion.Reason
	}
	want := map[string]string{
		"denied": FleetExclusionNotGranted, "offline": FleetExclusionOffline,
		"slow": FleetExclusionTimeout, "tampered": FleetExclusionInvalidSnapshot,
	}
	if len(reasons) != 5 {
		t.Fatalf("exclusions = %+v", view.Excluded)
	}
	for instance, reason := range want {
		if reasons[instance] != reason {
			t.Errorf("%s reason = %q, want %q", instance, reasons[instance], reason)
		}
	}
	if !strings.Contains(reasons["skew"], "feature version test/v2; fleet clustered test/v1") {
		t.Fatalf("version-skewed snapshot exclusion = %q", reasons["skew"])
	}
}

func TestCollectFleetSnapshotsBoundsConcurrency(t *testing.T) {
	var active, maximum atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 6)
	snapshots := map[string]FleetSnapshot{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		snapshots[id] = snapshotOf(t, "test/v1", id, "s", fleetT0)
	}
	fetch := func(ctx context.Context, target FleetTarget) (FleetSnapshot, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return snapshots[target.InstanceID], nil
		case <-ctx.Done():
			return FleetSnapshot{}, ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := CollectFleetSnapshots(context.Background(), fleetTargets("a", "b", "c", "d", "e", "f"), fetch, FleetCollectionOptions{
			MaxConcurrency: 2, PerInstanceTimeout: time.Second, OverallTimeout: time.Second,
		})
		done <- err
	}()
	<-started
	<-started
	select {
	case <-started:
		t.Fatal("third fetch started while two workers were blocked")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", maximum.Load())
	}
}

func TestCollectFleetSnapshotsEnforcesOverallDeadline(t *testing.T) {
	fetch := func(ctx context.Context, target FleetTarget) (FleetSnapshot, error) {
		<-ctx.Done()
		return FleetSnapshot{}, ctx.Err()
	}
	start := time.Now()
	snapshots, excluded, err := CollectFleetSnapshots(context.Background(), fleetTargets("a", "b", "c"), fetch, FleetCollectionOptions{
		MaxConcurrency: 1, PerInstanceTimeout: time.Minute, OverallTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 || len(excluded) != 3 {
		t.Fatalf("snapshots=%+v exclusions=%+v", snapshots, excluded)
	}
	for _, exclusion := range excluded {
		if exclusion.Reason != FleetExclusionTimeout {
			t.Fatalf("exclusion %+v, want timeout", exclusion)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("overall deadline took %v", elapsed)
	}
}

func TestCollectFleetSnapshotsCancellationStopsFetches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started sync.WaitGroup
	started.Add(2)
	stopped := make(chan struct{}, 2)
	fetch := func(ctx context.Context, target FleetTarget) (FleetSnapshot, error) {
		started.Done()
		<-ctx.Done()
		stopped <- struct{}{}
		return FleetSnapshot{}, ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := CollectFleetSnapshots(ctx, fleetTargets("a", "b"), fetch, FleetCollectionOptions{
			MaxConcurrency: 2, PerInstanceTimeout: time.Minute, OverallTimeout: time.Minute,
		})
		done <- err
	}()
	started.Wait()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("fetch did not stop after cancellation")
		}
	}
}

func TestClusterFleetOnDemandDoesNotWriteFiles(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	snapshot := snapshotOf(t, "test/v1", "ok", "s", fleetT0, blob("a", 0, 0, 4))
	if _, err := ClusterFleetOnDemand(context.Background(), fleetTargets("ok"), func(context.Context, FleetTarget) (FleetSnapshot, error) {
		return snapshot, nil
	}, FleetCollectionOptions{}, Options{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) || before[0].Name() != after[0].Name() {
		t.Fatalf("directory changed: before=%v after=%v", before, after)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("marker = %q, %v", data, err)
	}
}

func fleetTargets(ids ...string) []FleetTarget {
	targets := make([]FleetTarget, len(ids))
	for i, id := range ids {
		targets[i] = FleetTarget{InstanceID: id}
	}
	return targets
}

func fleetInstanceIDs(instances []FleetInstance) []string {
	ids := make([]string, len(instances))
	for i, instance := range instances {
		ids[i] = instance.InstanceID
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
